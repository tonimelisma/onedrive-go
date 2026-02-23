# E2E Sync Test Suite -- Consolidated Executive Summary

**Date**: 2026-02-22
**Commit**: c0e036d (main)
**Test environment**: macOS, live OneDrive (personal:testitesti18@outlook.com), drive BD50CF43646E28E6

---

## 1. Test Results Overview

**28 PASS / 8 FAIL** (77.8% pass rate)

| # | Test Name | Result | Duration | Key Finding |
|---|-----------|--------|----------|-------------|
| 1 | TestSyncE2E_InitialDownload | PASS | 26.06s | SQLITE_BUSY on 4/8 concurrent download DB writes; test passes only due to weak `>=3` assertion (should be `>=7`) |
| 2 | TestSyncE2E_InitialUpload | PASS | 13.95s | Upload works; no idempotency test; stale remote artifacts from prior runs |
| 3 | TestSyncE2E_IncrementalEditRemote | PASS | 16.66s | Three-way merge with hash comparison works correctly; delta token persistence verified |
| 4 | TestSyncE2E_IncrementalEditLocal | PASS | 15.59s | Enrichment guard not exercised; 17 folder synced_updates every cycle because LastSyncedAt never set |
| 5 | TestSyncE2E_BidirectionalMerge | PASS | 20.09s | Weak assertions (`>=1` instead of exact counts); downloads 6 unrelated files from dirty drive |
| 6 | TestSyncE2E_AlreadyInSync | PASS | 16.34s | D1 unreachable for folders; D2 adopt fires every cycle instead; three-cycle steady-state proven |
| 7 | TestSyncE2E_IncrementalAddLocal | PASS | 14.19s | 17 synced_updates in cycle 2 confirm LastSyncedAt not set for folders |
| 8 | TestSyncE2E_IncrementalAddRemote | **FAIL** | 7.41s | SQLITE_BUSY during baseline sync; never reaches incremental add test |
| 9 | TestSyncE2E_MultipleCycles | PASS | 23.00s | D3 does not write synced baseline, forcing folder re-adoption every cycle |
| 10 | TestSyncE2E_DeleteRemoteFile | PASS | 20.35s | Scanner creates duplicate `local:` row for tombstoned file; file re-uploaded then deleted locally, leaving it present remotely |
| 11 | TestSyncE2E_DownloadOnly | PASS | 20.29s | SQLITE_BUSY on 1/6 downloads; two-layer enforcement (scan skip + reconciler override) |
| 12 | TestSyncE2E_FalseConflict | PASS | 20.84s | F4 false conflict correctly resolved; F10 (unsynced false conflict) not covered |
| 13 | TestSyncE2E_DeleteLocalFile | PASS | 16.91s | F6 correctly propagates local file deletion to remote; folder LastSyncedAt never set |
| 14 | TestSyncE2E_EditDeleteConflict | **FAIL** | 19.14s | Scanner splits state into two DB rows; F9 never fires; conflict detected by S4 not counted in report.Conflicts |
| 15 | TestSyncE2E_DeleteLocalFolder | PASS | 11.70s | No D7 rule for folder deletion propagation; folder re-created locally by D3 instead of deleted remotely |
| 16 | TestSyncE2E_CreateCreateConflict | PASS | 16.96s | F11 create-create conflict correctly resolved with keep-both-download; conflict download not counted in report.Downloaded |
| 17 | TestSyncE2E_EditEditConflict | PASS | 16.08s | F5 edit-edit conflict correctly resolved; conflict download bypasses TransferManager, bytes not counted |
| 18 | TestSyncE2E_DeleteRemoteFolder | PASS | 27.02s | Scanner creates fresh `local:` rows for files in deleted folder; content re-uploaded; folder tombstone never reaches D4 |
| 19 | TestSyncE2E_UploadOnly | **FAIL** | 14.97s | Upload-only mode skips delta fetch, no root item in DB; resolveParentID returns `local:` IDs to Graph API, HTTP 400 |
| 20 | TestSyncE2E_DryRun | PASS | 10.20s | Dry-run skips execution but delta token and item state ARE persisted to DB |
| 21 | TestSyncE2E_NosyncGuard | PASS | 6.32s | .nosync guard halts sync; delta fetch runs before guard fires (guard is in scanner, not engine) |
| 22 | TestSyncE2E_DryRunNoSideEffects | PASS | 15.56s | Dry-run followed by real sync works correctly; dry-run advances delta cursor |
| 23 | TestSyncE2E_DeepNesting | PASS | 14.56s | 4-level directory chain uploaded; sequential folder creation ~0.6-0.8s per API call per level |
| 24 | TestSyncE2E_BigDeleteBlocked | PASS | 19.67s | S5 big-delete protection triggers correctly; total item count polluted by drive-wide state |
| 25 | TestSyncE2E_BigDeleteForce | **FAIL** | 11.78s | SQLITE_BUSY during baseline upload of 15 files; 3/15 uploads lose DB state |
| 26 | TestSyncE2E_NewLocalFolder | PASS | 14.95s | D5 (create folder remotely) works; B-050 cleanup correctness-critical for resolveParentID chain |
| 27 | TestSyncE2E_NewRemoteFolder | PASS | 25.50s | D3 (create folder locally) from incremental delta works; D1 structurally dead code for folders |
| 28 | TestSyncE2E_SkipFilesPattern | PASS | 10.85s | skip_files filter works at scanner layer; filter not applied on remote/delta side |
| 29 | TestSyncE2E_SkipDotfiles | PASS | 11.47s | skip_dotfiles filter works; no test for remote dotfiles being downloaded |
| 30 | TestSyncE2E_SkipDirsPattern | PASS | 13.72s | skip_dirs prevents directory descent entirely; no test for nested pattern matching |
| 31 | TestSyncE2E_QuietMode | **FAIL** | 8.09s | runSyncRaw hardcodes `--verbose` conflicting with `--quiet`; exitOnError ignores `--quiet` flag |
| 32 | TestSyncE2E_JSONOutputFormat | PASS | 8.60s | JSON output well-formed; `errors` field correctly serialized as `[]` not `null` |
| 33 | TestSyncE2E_LargeFile | **FAIL** | 41.27s | 5 MiB file upload times out; http.Client.Timeout=30s too short; no retry on chunk timeout |
| 34 | TestSyncE2E_ExitCodeOnErrors | **FAIL** | 7.98s | SQLITE_BUSY during concurrent downloads causes non-zero exit; test never validates exit-code logic |
| 35 | TestSyncE2E_UnicodeFilenames | PASS | 18.32s | Japanese filename correctly handled; url.PathEscape encodes multi-byte characters properly |
| 36 | TestSyncE2E_SpacesInFilenames | **FAIL** | 8.29s | SQLITE_BUSY during concurrent downloads; spaces handling works correctly but test fails before verification |

**Total wall-clock time**: ~570 seconds (~9.5 minutes)

---

## 2. Sync Engine Health Assessment

### Overall Verdict: FUNCTIONAL BUT FRAGILE

The sync engine's core algorithm -- delta fetch, local scan, three-way reconcile, execute -- is architecturally sound and handles the happy path correctly. The reconciler's 14-rule file decision matrix (F1-F14) and 6-rule folder matrix (D1-D6) produce correct actions for the common cases: initial upload, initial download, incremental edits (local and remote), bidirectional merge, false conflicts, and filter enforcement.

However, the engine has **three categories of structural defects** that prevent it from being production-reliable:

1. **Infrastructure reliability** (SQLITE_BUSY): The state store cannot handle concurrent writes from the transfer worker pool. This is not a rare edge case -- it fires reliably when 3+ downloads complete within the same millisecond, which happens in almost every initial sync with small files.

2. **Deletion correctness** (scanner state splitting, missing D7): The scanner's interaction with tombstoned items creates duplicate DB rows that cause the reconciler to re-upload deleted files instead of propagating deletions. Local folder deletions are silently reversed. These are data-correctness bugs that would cause user-visible sync failures.

3. **Operational gaps** (upload-only bootstrap, large file timeout, quiet mode): Several operational modes are non-functional due to missing initialization steps or incorrect timeout configuration.

---

## 3. Critical Bugs

### BUG-1: SQLITE_BUSY -- No busy_timeout Pragma (SEVERITY: HIGH)

**Impact**: Directly causes 5 test failures; observed in 3 additional passing tests. Any sync with concurrent downloads/uploads will intermittently lose DB state.

**Mechanism**: `TransferManager.dispatchPool` dispatches up to 8 goroutines via `errgroup`. Each goroutine calls `store.UpsertItem` upon completing a transfer. `SQLiteStore` is opened via `sql.Open("sqlite", dbPath)` without `db.SetMaxOpenConns(1)` and without `PRAGMA busy_timeout`. When multiple goroutines acquire write locks simultaneously (common with small files completing within the same millisecond), SQLite returns `SQLITE_BUSY (5)` immediately rather than waiting.

**Evidence** (from TestSyncE2E_ExitCodeOnErrors log):
```
level=WARN msg="transfer: action skipped" path=manual-test/test-copy.txt
  error="upsert item BD50CF43646E28E6/BD50CF43646E28E6!sa6a886096e094d1abbf3f6b1d33d2b8e: database is locked (5) (SQLITE_BUSY)"
```
Three downloads complete at 00:24:58.774-777, all calling UpsertItem concurrently. Two of three get SQLITE_BUSY.

**Affected tests**: IncrementalAddRemote (FAIL), BigDeleteForce (FAIL), ExitCodeOnErrors (FAIL), SpacesInFilenames (FAIL), InitialDownload (PASS with weak assertions), DownloadOnly (PASS with 1 skipped), QuietMode (FAIL, cascading).

**Consequence**: Files are physically downloaded to disk but their DB state is not recorded. On the next sync cycle, the engine sees them as new local files and re-uploads them to OneDrive (a false upload). The sync report shows `errors=N`, causing non-zero exit codes.

**Fix**: Add `PRAGMA busy_timeout = 5000` to `setPragmas` in `state.go`. Additionally, call `db.SetMaxOpenConns(1)` after `sql.Open` as a belt-and-suspenders measure.

**Files**: `internal/sync/state.go` (setPragmas), `internal/sync/state.go` (NewStore)

---

### BUG-2: Scanner State Splitting -- Duplicate `local:` Rows for Tombstoned Items (SEVERITY: HIGH)

**Impact**: File deletions from remote and edit-delete conflicts produce incorrect behavior. Files that should be deleted locally are instead re-uploaded. The F9 (edit-delete conflict) reconciler path is structurally unreachable.

**Mechanism**: When the delta processor inserts a tombstone (deleted=true item), it updates the existing DB row to set `Deleted=true` and clear hashes. When the scanner subsequently walks the local filesystem, it finds the file still on disk (not yet deleted) and calls `handleNewFile`. Because the tombstoned row has `Deleted=true`, the `getItemByPath` lookup fails to find a "live" item, so the scanner creates a **new** `local:` prefixed row alongside the tombstoned row. The reconciler now sees two separate items: the tombstone (which triggers F8: remote deleted, local unchanged) and the `local:` item (which triggers F3: new local file, upload).

The net result is: the file is deleted locally (F8) AND re-uploaded to OneDrive (F3). The user's intent (delete the file remotely) is reversed -- the file reappears on OneDrive.

**Evidence** (from TestSyncE2E_DeleteRemoteFile report):
```
# Scanner creates fresh local: row for the file it found on disk:
level=DEBUG msg="scanner: new local file" path=sync-e2e-.../delete-me.txt size=21
level=DEBUG msg="upserting item" item_id="local:sync-e2e-.../delete-me.txt"

# Reconciler classifies tombstone and local: row independently:
level=DEBUG msg="F8: remote deleted, local unchanged -> delete locally" path=...
level=DEBUG msg="F3/F12: local changed -> upload" path=...
```

**Affected tests**: DeleteRemoteFile (PASS but wrong behavior), EditDeleteConflict (FAIL), DeleteRemoteFolder (PASS but wrong behavior).

**Consequence**: Remote deletions are silently reversed. Edit-delete conflicts never fire the F9 path. Files deleted on another device reappear after sync. This is a **data correctness** bug that would cause user-visible sync loops.

**Fix**: The scanner's `handleNewFile` must check for tombstoned items at the same path before creating a `local:` row. If a tombstone exists, the scanner should either (a) attach the local state to the tombstoned row (setting LocalHash, LocalSize, LocalMtime) so the reconciler sees both local and remote state on a single item, or (b) skip the file entirely and let the tombstone-processing path handle it.

**Files**: `internal/sync/scanner.go` (handleNewFile, getItemByPath)

---

### BUG-3: Missing Folder Deletion Propagation Rule D7 (SEVERITY: HIGH)

**Impact**: Local folder deletions are silently reversed instead of propagated to OneDrive.

**Mechanism**: The reconciler's folder decision matrix has rules D1-D6 but no D7 (folder deleted locally, propagate deletion to remote). When a user deletes a local folder that was previously synced, the next delta fetch returns the folder (still present remotely). The reconciler classifies it as D3 (remote-only folder, create locally), which re-creates the folder on disk. The user's deletion is undone.

**Evidence** (from TestSyncE2E_DeleteLocalFolder report):
```
# After local folder deletion and sync:
level=DEBUG msg="D3: create folder locally" path=sync-e2e-.../local-folder-to-delete
# The folder is re-created instead of deleted remotely
```

The test passes because it only asserts that the folder exists after sync (which it does, because D3 re-creates it), but the test comment says "verify folder is deleted remotely" while the assertion checks the opposite.

**Consequence**: Users cannot delete folders via local filesystem operations. Deleted folders reappear after every sync cycle. This directly contradicts the bidirectional sync contract.

**Fix**: Add a D7 rule to the reconciler that detects: folder exists in DB with SyncedHash set, folder not found on local disk, folder present in remote delta. Action: delete folder remotely. This mirrors F6 (file deleted locally, propagate to remote) but for folders.

**Files**: `internal/sync/reconciler.go` (classifyFolder decision matrix)

---

### BUG-4: Upload-Only Mode Missing Root Item Bootstrap (SEVERITY: MEDIUM)

**Impact**: Upload-only mode (`--upload-only`) is completely non-functional. All uploads fail with HTTP 400.

**Mechanism**: Upload-only mode skips the delta fetch phase (by design, to avoid downloading). However, the delta fetch is also responsible for populating the root drive item in the DB. Without the root item, `resolveParentID` cannot find the parent for top-level files. It falls back to constructing a `local:` prefixed ID and passes it to the Graph API, which rejects it with HTTP 400 (Bad Request).

**Evidence** (from TestSyncE2E_UploadOnly log):
```
level=ERROR msg="failed to resolve parent for upload"
  path=sync-e2e-.../upload-only-test.txt
  error="parent item not found in DB"
```

**Fix**: In upload-only mode, perform a minimal root item fetch (GET `/drives/{id}/root`) to populate the root item in the DB before running the scanner. Alternatively, use the `/drives/{id}/items/{parentId}:/filename:/content` path pattern that does not require a DB-resolved parent ID.

**Files**: `internal/sync/engine.go` (RunOnce, upload-only branch), `internal/sync/executor.go` (resolveParentID)

---

### BUG-5: HTTP Client Timeout Too Short for Large File Uploads (SEVERITY: MEDIUM)

**Impact**: Files larger than ~4 MiB reliably fail to upload on connections slower than ~175 KB/s.

**Mechanism**: `http.Client.Timeout = 30s` (hardcoded in `root.go:39`) is a whole-request timeout covering TCP connect + TLS + body send + response wait. For a 5 MiB chunked upload to OneDrive's CDN (`my.microsoftpersonalcontent.com`), 30 seconds is insufficient if upload bandwidth is limited. The timeout fires before the server acknowledges the PUT, and `UploadChunk` does not retry because it calls `c.httpClient.Do(req)` directly (bypassing the retry-equipped `c.Do()` method).

**Evidence** (from TestSyncE2E_LargeFile log):
```
time=00:23:40.487 level=DEBUG msg="uploading chunk" offset=0 length=5242880 total=5242880
time=00:24:10.489 level=ERROR msg="chunk upload request failed"
  error="Put \"https://my.microsoftpersonalcontent.com/...\": context deadline exceeded
  (Client.Timeout exceeded while awaiting headers)"
```
Exactly 30 seconds between chunk start and timeout.

**Fix**: (a) Use `ConnectTimeout` and `DataTimeout` config fields (already defined but unused) to configure a custom `http.Transport` with per-phase timeouts. (b) For chunk uploads specifically, compute a dynamic timeout based on chunk size and a minimum-bandwidth assumption (e.g., 50 KB/s). (c) Add retry logic in `uploadChunked` that queries the upload session for accepted ranges and resumes.

**Files**: `root.go` (defaultHTTPClient, httpClientTimeout), `internal/sync/executor.go` (uploadChunked), `upload.go` (UploadChunk), `internal/config/config.go` (ConnectTimeout, DataTimeout -- dead code)

---

### BUG-6: Quiet Mode Test Helper Conflict (SEVERITY: LOW)

**Impact**: TestSyncE2E_QuietMode fails because the test helper hardcodes `--verbose`.

**Mechanism**: `runSyncRaw` in `sync_helpers_test.go` always appends `"--verbose"` to the command args. When the test passes `"--quiet"`, the binary receives both `--verbose` and `--quiet`. Cobra processes `--verbose` last (or the slog level is set by whichever flag is processed last), resulting in verbose output despite `--quiet` being specified.

Additionally, `exitOnError` in `sync.go` does not check the `--quiet` flag before printing error messages to stderr.

**Fix**: (a) Make the test helper conditionally append `--verbose` only when `--quiet` is not in the args. (b) Have `exitOnError` respect the `--quiet` flag.

**Files**: `e2e/sync_helpers_test.go` (runSyncRaw), `sync.go` (exitOnError)

---

### BUG-7: Folder D1 Rule Structurally Unreachable (SEVERITY: LOW)

**Impact**: Folders are re-adopted (D2) and re-synced-updated every cycle instead of being recognized as already-in-sync (D1). This causes unnecessary DB writes and log noise but no data corruption.

**Mechanism**: D1 requires `LastSyncedAt > 0` for a folder. However, neither `executeFolderCreate` nor `executeSyncedUpdate` ever sets `LastSyncedAt` on folder items. Folders always have `LastSyncedAt == 0`, so D1's condition is never met. Instead, D2 (adopt folder) fires every cycle for every folder.

**Evidence** (from TestSyncE2E_AlreadyInSync, cycle 3):
```
# 17 folders re-adopted and re-synced-updated even on cycle 3:
level=INFO msg="reconciliation complete" synced_updates=17
```

**Fix**: Set `LastSyncedAt = NowNano()` in `executeFolderCreate` and `executeSyncedUpdate` when processing folder items.

**Files**: `internal/sync/executor.go` (executeFolderCreate, executeSyncedUpdate)

---

## 4. Subsystem Coverage Map

### 4.1 Delta Processor

| Capability | Tested | Tests | Notes |
|-----------|--------|-------|-------|
| Initial full delta fetch | Yes | InitialDownload, InitialUpload, all first-cycle tests | Works correctly; 24 items returned from drive root |
| Incremental delta (with token) | Yes | IncrementalEditRemote, IncrementalAddRemote, MultipleCycles | Delta token persisted and reused correctly |
| `decodeURLEncodedNames` | No (E2E) | Unit tests only (normalize_test.go) | Japanese filename entered via scanner, not delta; delta-side decoding not exercised at E2E level |
| `filterPackages` | No | -- | No OneDrive Packages (OneNote notebooks) in test account |
| `deduplicateItems` | No | -- | No duplicate items observed in test deltas |
| Delta pagination (`nextLink`) | No | -- | All test deltas fit in single page (24 items) |

### 4.2 Scanner

| Capability | Tested | Tests | Notes |
|-----------|--------|-------|-------|
| New local file detection | Yes | InitialUpload, IncrementalAddLocal, all upload tests | `local:` prefix ID assignment works correctly |
| NFC normalization | Yes | UnicodeFilenames | `norm.NFC.String()` applied before DB storage |
| QuickXorHash computation | Yes | All upload tests | Hash computed via `computeHash` and matched post-upload |
| `.nosync` guard | Yes | NosyncGuard | Guard fires at scanner level, halts sync |
| Orphan detection | Partial | DeleteLocalFile, DeleteLocalFolder | File orphans detected; folder orphans cause D7 gap |
| Tombstone interaction | **BROKEN** | DeleteRemoteFile, EditDeleteConflict, DeleteRemoteFolder | Scanner creates duplicate rows for tombstoned items (BUG-2) |
| Skip filters (files) | Yes | SkipFilesPattern | Scanner-layer filtering works; no remote-side filter |
| Skip filters (dirs) | Yes | SkipDirsPattern | Directory descent prevented entirely |
| Skip dotfiles | Yes | SkipDotfiles | Local dotfiles skipped; remote dotfiles not tested |
| Upload-only scan | **BROKEN** | UploadOnly | Scanner runs but resolveParentID fails (BUG-4) |
| Download-only scan skip | Yes | DownloadOnly | Scanner runs but uploads suppressed by reconciler override |

### 4.3 Reconciler

| Rule | Description | Tested | Tests | Notes |
|------|------------|--------|-------|-------|
| F1 | Already in sync | Yes | AlreadyInSync, MultipleCycles | Works for files; cycles 2+ prove steady state |
| F2 | Remote changed only | Yes | IncrementalEditRemote | Three-way hash comparison correct |
| F3 | Local changed only / new local file | Yes | IncrementalEditLocal, IncrementalAddLocal, InitialUpload | Upload path works correctly |
| F4 | False conflict (same content) | Yes | FalseConflict | Hash equality detected, no conflict generated |
| F5 | Edit-edit conflict | Yes | EditEditConflict | Keep-both-download resolution works |
| F6 | Local file deleted, propagate remote | Yes | DeleteLocalFile | Remote DELETE issued correctly |
| F7 | -- | -- | -- | -- |
| F8 | Remote deleted, local unchanged | Partial | DeleteRemoteFile | Fires correctly but scanner creates parallel F3 (BUG-2) |
| F9 | Edit-delete conflict | **UNREACHABLE** | EditDeleteConflict | Scanner state splitting prevents F9 from firing (BUG-2) |
| F10 | Unsynced false conflict | No | -- | No test creates this scenario |
| F11 | Create-create conflict | Yes | CreateCreateConflict | Keep-both-download resolution works |
| F12 | New local file (upload) | Yes | Same as F3 | -- |
| F13-F14 | -- | -- | -- | -- |
| D1 | Folder already in sync | **UNREACHABLE** | AlreadyInSync, NewRemoteFolder | LastSyncedAt never set for folders (BUG-7) |
| D2 | Adopt remote folder found locally | Yes | All tests | Fires correctly; fires every cycle due to D1 gap |
| D3 | Create folder locally | Yes | All tests with remote folders | Works; 17 folders created in initial sync |
| D4 | Folder deleted remotely | Partial | DeleteRemoteFolder | Tombstone never reaches D4 due to scanner state splitting (BUG-2) |
| D5 | Create folder remotely | Yes | NewLocalFolder, DeepNesting | Works; B-050 cleanup verified |
| D6 | -- | -- | -- | -- |
| D7 | Local folder deleted, propagate remote | **MISSING** | DeleteLocalFolder | Rule does not exist; D3 reverses deletion (BUG-3) |

### 4.4 Safety Checks

| Check | Description | Tested | Tests | Notes |
|-------|------------|--------|-------|-------|
| S1 | Max actions threshold | No | -- | Default threshold not hit in any test |
| S2 | -- | -- | -- | -- |
| S3 | -- | -- | -- | -- |
| S4 | Conflict detection | Partial | EditDeleteConflict | Triggers but conflict not counted in report.Conflicts |
| S5 | Big-delete protection | Yes | BigDeleteBlocked | Correctly blocks >50% deletion; item count polluted by drive-wide state |
| S5+force | Big-delete force override | **FAIL** | BigDeleteForce | SQLITE_BUSY during baseline prevents reaching force logic |
| S6-S7 | -- | -- | -- | -- |

### 4.5 Executor

| Phase | Tested | Tests | Notes |
|-------|--------|-------|-------|
| folder_creates | Yes | All tests | 17 folders created via os.MkdirAll; sequential, reliable |
| moves | No | -- | No test triggers move detection |
| downloads | Yes | InitialDownload, IncrementalEditRemote | Parallel (8 workers); SQLITE_BUSY under contention (BUG-1) |
| uploads (simple) | Yes | InitialUpload, IncrementalEditLocal | Simple PUT for files <= 4 MiB; works correctly |
| uploads (chunked) | **FAIL** | LargeFile | 30s http.Client.Timeout kills 5 MiB upload (BUG-5) |
| local_deletes | Yes | DeleteRemoteFile, DeleteLocalFile | File deletion works; folder deletion absent (BUG-3) |
| remote_deletes | Yes | DeleteLocalFile | DELETE API call works correctly |
| conflicts | Yes | EditEditConflict, CreateCreateConflict | Keep-both resolution works; bytes not counted in report |
| synced_updates | Yes | All multi-cycle tests | Fires every cycle for all folders (BUG-7) |
| cleanups | Yes | DryRunNoSideEffects | WAL checkpoint runs; tombstone cleanup (0 deleted typical) |

### 4.6 Conflict Handler

| Scenario | Tested | Tests | Notes |
|----------|--------|-------|-------|
| Edit-edit (F5) | Yes | EditEditConflict | Keep-both-download with timestamped conflict copy |
| Create-create (F11) | Yes | CreateCreateConflict | Keep-both-download works |
| Edit-delete (F9) | **UNREACHABLE** | EditDeleteConflict | Scanner state splitting prevents F9 (BUG-2) |
| False conflict (F4) | Yes | FalseConflict | Same-hash detection works |
| Conflict counting in report | **BROKEN** | EditDeleteConflict | S4 detects conflict but report.Conflicts is 0 |

### 4.7 Transfer Manager

| Capability | Tested | Tests | Notes |
|-----------|--------|-------|-------|
| Parallel downloads (errgroup) | Yes | InitialDownload, all download tests | 8 workers; works but SQLITE_BUSY under contention |
| Parallel uploads (errgroup) | Yes | BigDeleteForce (attempted) | 8 workers; same SQLITE_BUSY issue as downloads |
| Bandwidth limiting | No | -- | BandwidthLimiter exists but no test exercises rate caps |
| Download hash verification | Yes | All download tests | `gotHash != action.Item.QuickXorHash` checked post-download |
| Upload hash verification | **GAP** | -- | Local hash computed but not compared against server response |

### 4.8 Filter Engine

| Filter | Tested | Tests | Notes |
|--------|--------|-------|-------|
| skip_files glob | Yes | SkipFilesPattern | Scanner-layer; `*.log` pattern works |
| skip_dirs glob | Yes | SkipDirsPattern | Prevents directory descent entirely |
| skip_dotfiles | Yes | SkipDotfiles | Local dotfiles skipped; remote dotfiles not tested |
| sync_paths restriction | No | -- | No test uses sync_paths; all tests sync entire drive root |
| Remote-side filtering | No | -- | Filters only applied during local scan, not delta processing |

### 4.9 State Store (SQLite)

| Capability | Tested | Tests | Notes |
|-----------|--------|-------|-------|
| Migration v1 | Yes | All tests | Applied on every fresh DB |
| UpsertItem | Yes | All tests | Works for single-threaded; SQLITE_BUSY under concurrency (BUG-1) |
| GetItemByPath | Yes | Scanner tests | Used for duplicate detection |
| DeleteItem | Yes | Upload tests (B-050 cleanup) | `local:` row deletion after upload works |
| WAL mode | Yes | All tests | Enabled via pragma |
| busy_timeout | **MISSING** | -- | Not set; causes BUG-1 |
| Concurrent write safety | **BROKEN** | InitialDownload, BigDeleteForce, etc. | No SetMaxOpenConns(1), no busy_timeout |

---

## 5. Cross-Cutting Patterns

### Pattern 1: SQLITE_BUSY Is the Dominant Failure Mode

5 of 8 test failures are directly caused by SQLITE_BUSY. An additional 3 passing tests have observed SQLITE_BUSY errors that are masked by weak assertions. This single infrastructure bug accounts for 62.5% of all test failures. Fixing `busy_timeout` alone would likely bring the pass rate from 77.8% to 91.7% (33/36).

### Pattern 2: Scanner-Reconciler Interaction for Deletions Is Fundamentally Broken

The scanner creates fresh `local:` rows for files that have tombstoned DB entries. This causes three distinct bugs:
- Remote file deletions are reversed (file re-uploaded)
- Edit-delete conflicts never fire their intended code path (F9 unreachable)
- Remote folder deletions cause content re-upload

All three share the same root cause: the scanner's `handleNewFile` does not check for tombstoned items at the same path.

### Pattern 3: Folder Lifecycle Is Incomplete

Folders are second-class citizens in the sync engine:
- D1 (already in sync) is unreachable because LastSyncedAt is never set
- D7 (local folder deletion propagation) does not exist
- D4 (remote folder deletion) is unreachable due to scanner state splitting
- D2 (adopt folder) fires every cycle as a workaround for the D1 gap
- 17 synced_updates fire every cycle for all folders in the drive

### Pattern 4: Test Suite Syncs Entire Drive Root

Every test in the suite syncs the full OneDrive drive root (no `sync_paths` restriction). This means:
- 24 items are fetched in every initial delta (including stale test artifacts)
- 5+ unrelated downloads occur in every test
- Download concurrency triggers SQLITE_BUSY (amplifying Pattern 1)
- Safety check item counts are polluted by drive-wide state
- Test failures are caused by pre-existing remote content, not test logic

### Pattern 5: Assertions Are Frequently Weak

Multiple passing tests have assertions that are too loose to catch real bugs:
- InitialDownload: `assert.GreaterOrEqual(t, report.Downloaded, 3)` when 8 files exist (should be `>=7`)
- BidirectionalMerge: `>=1` instead of exact upload/download counts
- DeleteRemoteFile: Asserts file is absent locally but does not check remote (file was re-uploaded)
- DeleteLocalFolder: Asserts folder exists (re-created by D3) instead of asserting remote deletion
- BigDeleteBlocked: S5 threshold calculation uses polluted drive-wide item count

---

## 6. Test Suite Quality Assessment

### Strengths

1. **Comprehensive scenario coverage**: 36 tests cover initial sync, incremental edits, bidirectional merge, all three conflict types, three filter types, dry-run, big-delete protection, deep nesting, Unicode, spaces, large files, JSON output, exit codes, and quiet mode. This is an excellent breadth of coverage for an E2E suite.

2. **Isolation infrastructure**: `newSyncEnv` creates per-test temp dirs, fake HOME, isolated state DB, and uniquely-named remote folders with nanosecond timestamps. Token copying and cleanup are well-implemented.

3. **Multi-cycle testing**: Several tests (AlreadyInSync, MultipleCycles, DryRunNoSideEffects) run 2-3 sync cycles to verify steady-state behavior, which catches regressions that single-cycle tests miss.

4. **Content integrity verification**: Tests that verify file content use byte-exact assertions (`assert.Equal(t, expected, string(downloaded))`) via the `get` CLI command, providing strong end-to-end integrity guarantees.

5. **Log-level verification**: Tests capture and can inspect structured log output, enabling detailed failure diagnosis without re-running.

### Weaknesses

1. **No test isolation from drive state**: All tests sync the entire drive root, meaning pre-existing remote artifacts (manual-test/, stale sync-e2e-* folders) affect every test. Tests are not independent -- they share a dirty drive.

2. **Weak assertions mask real bugs**: Several tests pass despite incorrect behavior (DeleteRemoteFile, DeleteLocalFolder, InitialDownload) because assertions check insufficient conditions. A passing test does not imply correct behavior.

3. **No negative assertion for side effects**: Tests rarely assert that something did NOT happen. For example, DeleteRemoteFile should assert the file is NOT present remotely after sync, but it only checks local absence.

4. **Test helper hardcodes `--verbose`**: `runSyncRaw` always appends `--verbose`, making it impossible to test `--quiet` mode without the test helper itself being fixed.

5. **No tests for moves**: The reconciler and executor have move detection/execution logic, but no E2E test exercises file or folder moves.

6. **No tests for resume/retry**: The upload session resume and chunk retry paths are untested at the E2E level. The `QueryUploadSession` and `CancelUploadSession` methods are never called on error.

7. **Stale remote artifact accumulation**: `t.Cleanup` only removes the current test's remote folder. If cleanup fails (panic, timeout), folders accumulate at the drive root, polluting future test runs.

---

## 7. Top Risk Flags

### Risk 1: Data Loss via Silent Deletion Reversal (HIGH)

BUG-2 (scanner state splitting) and BUG-3 (missing D7) mean that deletions -- the most sensitive sync operation -- are silently reversed. A user who deletes a file or folder locally will see it reappear after the next sync. A user whose collaborator deletes a file remotely will see it re-uploaded. Neither user is warned. This is the highest-priority correctness risk.

### Risk 2: Split-State Items After SQLITE_BUSY (HIGH)

When SQLITE_BUSY causes a DB upsert to fail after a successful download, the file exists on disk but has no DB record. On the next sync, the scanner sees it as a new local file and uploads it -- creating a duplicate on OneDrive or triggering a false create-create conflict. With 8 concurrent workers and no busy_timeout, this happens reliably under normal load.

### Risk 3: Upload-Only Mode Non-Functional (MEDIUM)

Upload-only mode (`--upload-only`) is completely broken due to the missing root item bootstrap. Any user who tries this mode will get HTTP 400 errors on every file. This is a shipped CLI flag that does not work.

### Risk 4: Large Files Cannot Be Uploaded (MEDIUM)

The hardcoded 30-second HTTP timeout means any file upload that takes more than 30 seconds will fail. With no retry logic on chunk uploads, the failure is permanent for that sync cycle. The config fields for `ConnectTimeout` and `DataTimeout` exist but are dead code.

### Risk 5: Folder Sync Churn (LOW)

Every sync cycle produces 17+ synced_updates for all folders in the drive because D1 is unreachable. This is not a correctness issue but wastes DB writes, increases log noise, and would scale poorly with larger drives.

---

## 8. Recommendations

### Immediate (Fix Before Next Release)

1. **Add `PRAGMA busy_timeout = 5000` to `setPragmas` in `state.go`** and call `db.SetMaxOpenConns(1)` in `NewStore`. This single change fixes 5 test failures and eliminates the dominant reliability issue. Estimated: 2 lines of code.

2. **Fix scanner tombstone interaction in `handleNewFile`** (`scanner.go`). Before creating a `local:` row, check for an existing tombstoned item at the same path. If found, attach local state to the tombstoned row. This fixes BUG-2, unblocks F9, and corrects deletion behavior for files and folders.

3. **Add D7 folder deletion rule to the reconciler** (`reconciler.go`). Mirror the F6 file-deletion-propagation pattern for folders. This fixes BUG-3.

### Short-Term (Next Increment)

4. **Bootstrap root item in upload-only mode** (`engine.go`). Add a minimal `GET /drives/{id}/root` call at the start of RunOnce when mode is upload-only. This fixes BUG-4.

5. **Wire `ConnectTimeout`/`DataTimeout` config fields to HTTP client** (`root.go`). Replace the hardcoded `30s` timeout with a custom `http.Transport` that uses config-driven timeouts. For chunk uploads, compute a dynamic timeout based on chunk size. This fixes BUG-5.

6. **Add retry logic to `uploadChunked`** (`executor.go`). On chunk failure, query the upload session for accepted ranges and retry failed chunks. The session URL is valid for hours.

7. **Restrict test sync scope to test folder** (`sync_helpers_test.go`). Configure `sync_paths = ["/sync-e2e-<timestamp>"]` in `writeTestConfig`. This eliminates drive-wide state pollution, removes the SQLITE_BUSY amplifier, and makes tests independent of remote drive state.

### Medium-Term (Future Increments)

8. **Set `LastSyncedAt` for folders** in `executeFolderCreate` and `executeSyncedUpdate`. This unblocks D1 and eliminates the folder sync churn.

9. **Strengthen test assertions**. Replace `>=1` and `>=3` with exact expected counts. Add negative assertions (file NOT present remotely after deletion). Assert `report.Conflicts` counts for conflict tests.

10. **Add E2E tests for moves, resume, bandwidth limiting, and delta pagination**. These are untested subsystems that have production code paths.

11. **Add `classifyError` handling for SQLite errors** (`executor.go`). Recognize SQLITE_BUSY by error string or type and classify it as `ErrorRetry` rather than `ErrorSkip`.

12. **Fix test helper `--verbose` hardcoding** (`sync_helpers_test.go`). Conditionally append `--verbose` only when `--quiet` is not in the args.

13. **Clean up stale remote artifacts**. Add a test suite setup step that deletes all `sync-e2e-*` and `onedrive-go-e2e-*` folders at the drive root before running the suite.

---

## Appendix: Failure Classification

| Failure Mode | Test Failures | Root Bug |
|-------------|---------------|----------|
| SQLITE_BUSY (no busy_timeout) | IncrementalAddRemote, BigDeleteForce, ExitCodeOnErrors, SpacesInFilenames | BUG-1 |
| HTTP timeout (30s too short) | LargeFile | BUG-5 |
| Test helper conflict (--verbose + --quiet) | QuietMode | BUG-6 |
| Edit-delete conflict unreachable | EditDeleteConflict | BUG-2 |
| Upload-only missing root item | UploadOnly | BUG-4 |

Note: QuietMode also has a SQLITE_BUSY cascading failure, but the primary cause is the test helper conflict.

---

## Appendix: Passing Tests with Latent Bugs

| Test | Latent Bug | Why It Passes |
|------|-----------|---------------|
| InitialDownload | SQLITE_BUSY (4/8 downloads fail DB write) | Assertion is `>=3` instead of `>=7` |
| DeleteRemoteFile | Scanner state splitting; file re-uploaded | Test only asserts local absence, not remote absence |
| DeleteLocalFolder | Missing D7; folder re-created by D3 | Test asserts folder exists (wrong assertion) |
| DeleteRemoteFolder | Scanner state splitting; files re-uploaded | Test asserts local absence, not remote absence |
| DownloadOnly | SQLITE_BUSY (1/6 downloads fail DB write) | Assertion uses `>=` with slack |
| AlreadyInSync | D1 unreachable; D2 fires every cycle | Test does not assert 0 synced_updates |
| MultipleCycles | D3 does not write synced baseline | Test does not assert 0 folder re-adoptions |
