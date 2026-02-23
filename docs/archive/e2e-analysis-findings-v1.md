# Consolidated E2E Analysis Findings

**Source**: 36 agent-produced analysis reports from `/tmp/e2e-analysis/reports/`
**Scope**: Every unique observation, bug, code smell, misalignment, and architectural concern
**Convergence signals**: Noted when multiple independent reports identify the same issue

---

## 1. State Management Bugs

### 1.1 Scanner tombstone blindness / two-row split (CRITICAL)

**Convergence**: 3 reports independently identified this (DeleteRemoteFile, DeleteRemoteFolder, EditDeleteConflict)

When the delta processor tombstones a DB row (`MarkDeleted` sets `is_deleted=true`), the scanner's subsequent `GetItemByPath` call does not find the tombstoned row (tombstoned rows are excluded from active-item queries). The scanner therefore creates a fresh `local:` prefixed row for the same physical file/folder. The reconciler then sees two independent DB entries for the same path: the tombstoned row (triggering F8/D4) and the new scanner row (triggering F3/F12 or D5). This causes:

- **Files re-uploaded before being deleted locally** (DeleteRemoteFile): The file is uploaded to a new remote item ID, then the old tombstoned item triggers a local delete. Net result: file absent locally but present remotely -- the opposite of the intended behavior.
- **Folders re-created remotely before being deleted locally** (DeleteRemoteFolder): The scanner creates `local:` rows for both the folder and nested file. The executor creates the folder remotely (D5) and re-uploads the file (F3/F12) before the local delete phase runs.
- **F9 (edit-delete conflict) path unreachable** (EditDeleteConflict): The scanner writes the local edit into a new `local:` row instead of updating the tombstoned row's `LocalHash`. The tombstoned row retains stale `LocalHash == SyncedHash`, so `detectLocalChange` returns `false`, and the reconciler fires F8 (local unchanged) instead of F9 (local changed). The F9 code path is effectively dead code for this scenario.

**Code locations**:
- `scanner.go`: `GetItemByPath` call in `processFileEntry` / `processDirectoryEntry`
- `state.go`: `GetItemByPath` query filters `is_deleted=0`
- `reconciler.go:181-197`: `classifyRemoteTombstone` -- correct in isolation, but never sees `localChanged=true` on the tombstoned row
- `executor.go`: upload phase runs before local_deletes phase, amplifying the side effects

**Fix direction**: The scanner should check tombstoned rows when looking up paths. If a path matches a tombstone, the scanner should either update the tombstoned row's `LocalHash` (enabling F9) or suppress creating a new `local:` row (preventing the spurious upload).

---

### 1.2 Upload-only mode missing root item in DB (CRITICAL)

**Report**: UploadOnly

In `--upload-only` mode, the delta fetch is intentionally skipped (`engine.go:196`). The root item (ItemType=root, Path="") is only populated by the delta processor. Without it, `resolveParentID` for top-level folders calls `store.GetItemByPath(ctx, "")` and gets `nil`, failing with `"executor: parent folder "" not found in DB"`. The folder create is skipped, and the subsequent file upload retrieves the scanner's `local:` prefixed ID for the parent, which is sent to the Graph API as an HTTP 400 (`invalidRequest`).

**Code locations**:
- `engine.go:196-199`: `runDelta` short-circuit for upload-only
- `executor.go:699-727`: `resolveParentID` -- no fallback for missing root item
- `executor.go:702`: Partial `local:` guard -- checks `action.Item.ParentID` but NOT the DB-returned parent's `ItemID`

**Fix**: Either seed the root item via a minimal API call (`graph.GetItem(driveID, "root")`) on engine startup for upload-only mode, or add a lazy fallback in `resolveParentID`. Additionally, `resolveParentID` should guard against returning a `local:` prefixed ID from the DB lookup.

---

### 1.3 Dry-run has DB side effects

**Convergence**: 2 reports (DryRun, DryRunNoSideEffects)

The dry-run gate (`engine.go:216`) only bypasses the executor. Delta fetch writes 24+ item upserts and a delta token to the state DB. The scanner also writes `local:` items. On the next real sync, the delta starts from the saved token rather than doing a full re-enumeration. The test name `DryRunNoSideEffects` is misleading -- the DB has significant side effects.

**Code locations**:
- `engine.go:215-222`: dry-run gate placement (after all DB writes)
- `delta.go:153`: `finalizeDelta` saves token unconditionally
- `engine.go:156`: tombstone cleanup correctly has its own `!opts.DryRun` guard

This is arguably a design choice (progressive state), but it is undocumented. No test verifies post-dry-run DB state.

---

### 1.4 D2 re-adoption every cycle / folders never "synced"

**Convergence**: 7+ reports independently identified this (AlreadyInSync, DeleteLocalFile, IncrementalAddLocal, IncrementalEditLocal, IncrementalEditRemote, MultipleCycles, NewRemoteFolder)

`executeSyncedUpdate` for folders sets `SyncedHash = LocalHash` (both empty for folders) but never sets `LastSyncedAt`. Since `isSynced()` returns `SyncedHash != "" || LastSyncedAt != nil`, a folder without either is never considered "synced." Every sync cycle, all folders hit D2 (adopt) and produce spurious `synced_updates` DB writes. In cycle 2, this means 17+ unnecessary SQLite upserts.

**Code locations**:
- `executor.go:670-684`: `executeSyncedUpdate` -- does not set `LastSyncedAt` for folders
- `reconciler.go:427-429`: `isSynced` OR condition
- `reconciler.go:299`: D2 branch fires repeatedly

---

### 1.5 D3 does not write synced baseline for folders

**Report**: MultipleCycles

The D3 (create folder locally) phase creates local directories and upserts items but does not record a synced baseline. This forces a second cycle to do D2 adoption for all folders. Between cycle 1 and cycle 2, folders have no synced baseline, creating a window where modifications could be misclassified.

**Code location**: `executor.go`: `executeFolderCreate` for `FolderCreateLocal` -- sets `LocalMtime` but not `SyncedHash` or `LastSyncedAt`

---

### 1.6 B-050 crash-recovery gap

**Report**: NewLocalFolder

If the process crashes between `UpsertItem` (writing the server-assigned ID) and `DeleteItemByKey` (removing the stale `local:` row), the stale `local:` row remains in the DB alongside the real server ID row. Two rows for the same path would cause the reconciler to plan redundant actions.

**Code location**: `executor.go`: `executeFolderCreate` and `executeUpload` -- the upsert-then-delete is not atomic

---

### 1.7 Conflict download bytes not tracked

**Convergence**: 2 reports (CreateCreateConflict, EditEditConflict)

`executeConflict` calls `executeDownload` inline, but the returned byte count is discarded with `_`. The `SyncReport.BytesDownloaded` and `Downloaded` fields are not updated for conflict-resolution downloads. The engine log shows `downloaded=0` even when a real download occurred.

**Code location**: `executor.go:651-653`: `if _, err := e.executeDownload(ctx, sub); err != nil {`

---

### 1.8 handleLocalDeleteConflict bypasses report.Conflicts counter

**Report**: EditDeleteConflict

The S4 safety check in `handleLocalDeleteConflict` correctly detects file changes and creates conflict copies, but it runs within the `local_deletes` phase. The `dispatchPhase` `onSuccess` callback increments `s2.LocalDeleted`, not `s2.Conflicts`. The `report.Conflicts` field is only incremented in the `"conflicts"` phase handler. Conflicts detected through S4 are reported as local deletes, making them invisible in the JSON report.

**Code location**: `executor.go:572-607`: `handleLocalDeleteConflict`; `executor.go:158-160`: conflicts phase counter

---

### 1.9 ConflictType not persisted to DB

**Report**: EditEditConflict

`ConflictType` is documented as "in-memory only, not persisted to DB" (`types.go:94`). The `buildConflictRecord` function copies the type into the record, but if the DB schema does not store this field, it is silently discarded. A future `conflicts` CLI command (Phase 4.12) would not be able to show conflict type from stored records without a schema migration.

**Code location**: `types.go:94`, `conflict.go:139`

---

### 1.10 Files downloaded to disk but DB not updated after SQLITE_BUSY

**Convergence**: 6+ reports (BigDeleteForce, DownloadOnly, ExitCodeOnErrors, IncrementalAddRemote, InitialDownload, SpacesInFilenames)

When `UpsertItem` fails with SQLITE_BUSY after a successful download, the file exists on disk but has no DB record. On the next sync, the scanner will find these files locally with no `SyncedHash`, classifying them as new local files (F3/F12) and triggering redundant uploads. This leaves the DB in a split state.

**Code locations**: `executor.go`: `updateDownloadState` / `updateUploadState`; `transfer.go:153-158`: error recording

---

### 1.11 subfolder/tesmi ghost item persists across all cycles

**Convergence**: 30+ reports independently observed this

The item `subfolder/tesmi` has `localChanged=false remoteChanged=false synced=false localHash="" remoteHash="" syncedHash=""` in every sync cycle across all tests. It hits F1 (no-action) silently every time, consuming reconciler iterations without any cleanup trigger. The F14 cleanup requires `synced=true && localPresent=false && remotePresent=false`, but this item has `synced=false`, so F14 cannot fire. It appears to be mistyped as a file when it may be a folder, routed through `reconcileFile` instead of `reconcileFolder`.

**Code location**: `reconciler.go`: `reconcileItem` dispatch; `delta.go`: `convertGraphItem` item type inference

---

## 2. Concurrency Issues

### 2.1 SQLITE_BUSY from parallel transfer workers (CRITICAL)

**Convergence**: 6+ reports independently identified this (BigDeleteForce, DownloadOnly, ExitCodeOnErrors, IncrementalAddRemote, InitialDownload, SpacesInFilenames)

The `TransferManager` dispatches up to 8 goroutines via `errgroup`. Each goroutine, upon completing a transfer, calls `store.UpsertItem` concurrently. SQLite WAL mode allows only one writer at a time. Without a `busy_timeout` pragma, concurrent `UpsertItem` calls return `SQLITE_BUSY (error code 5)` immediately rather than waiting.

**Observed counts**:
- InitialDownload: 4 of 8 downloads failed DB update
- BigDeleteForce: 3 of 15 uploads failed DB update
- ExitCodeOnErrors: 2 of 5 downloads failed
- SpacesInFilenames: 1 of 5 downloads failed
- DownloadOnly: 1 of 6 downloads failed
- IncrementalAddRemote: baseline sync failed before reaching test logic

**Code locations**:
- `state.go`: `NewStore` / `setPragmas` -- no `busy_timeout` pragma set
- `state.go`: No `db.SetMaxOpenConns(1)` call
- `transfer.go:88,130`: 8-worker `errgroup` pool
- `executor.go`: `updateDownloadState` / `updateUploadState` -- no write serialization

**Fix**: Add `PRAGMA busy_timeout = 5000` to `setPragmas` in `state.go`. Additionally or alternatively, call `db.SetMaxOpenConns(1)`.

---

### 2.2 classifyError does not handle SQLite errors

**Convergence**: 6+ reports (same as 2.1)

`classifyError` (`executor.go:794-825`) has no case for SQLite errors. SQLITE_BUSY is a wrapped opaque string, not a typed sentinel. It falls through all recognized sentinels (`graph.ErrUnauthorized`, `graph.ErrThrottled`, `graph.ErrForbidden`, etc.) to the default `return ErrorSkip`. A DB write failure is fundamentally different from a transient Graph API error -- it should be classified as `ErrorRetryable` or at minimum handled distinctly.

**Code location**: `executor.go:794-825`

---

### 2.3 No retry logic for transient SQLITE_BUSY

**Report**: SpacesInFilenames (and implied by all SQLITE_BUSY reports)

The `UpsertItem` call in `updateDownloadState`/`updateUploadState` propagates the error directly to the transfer pool, which classifies it and skips the action. There is no retry wrapper around SQLite write operations. A retry with exponential backoff would prevent transient lock contention from manifesting as a skipped file.

---

### 2.4 Conflict ID uses nanosecond timestamp (collision risk)

**Report**: CreateCreateConflict

`buildConflictRecord` generates `ID = fmt.Sprintf("conflict-%d", NowNano())`. If two conflicts were resolved in the same nanosecond (possible if conflict resolution is ever parallelized), the IDs could collide. Currently safe because `dispatchPhase` is sequential.

**Code location**: `conflict.go`: `buildConflictRecord`

---

### 2.5 Crash-recovery gap in conflict resolution

**Report**: EditEditConflict

Between `os.Rename` (moving local file to conflict path) and `executeDownload` (downloading remote version), if the engine crashes, the local file would be at the conflict copy path and the original path would be empty. There is no checkpoint between the rename and the download.

**Code location**: `conflict.go:85-110`: `resolveKeepBothDownload`

---

## 3. Module Boundary Violations

### 3.1 exitOnError ignores --quiet flag

**Report**: QuietMode

`exitOnError` in `root.go:198-201` writes to stderr unconditionally via `fmt.Fprintf(os.Stderr, "Error: %v\n", err)` regardless of whether `--quiet` is set. If `--quiet` is intended to suppress all non-error output, this may be correct (errors should always be visible), but the test's `assert.Empty(t, stderr)` assertion assumes complete silence.

**Code location**: `root.go:198-201`

---

### 3.2 runSyncRaw hardcodes --verbose

**Report**: QuietMode

`runSyncRaw` (`sync_helpers_test.go:131`) always includes `--verbose` in the argument list. When testing `--quiet`, both flags are set simultaneously. `flagQuiet` wins (evaluated last in `buildLogger`), suppressing all diagnostic output. This means when a quiet-mode test fails, there is no verbose output available for diagnosis.

**Code location**: `sync_helpers_test.go:131`

---

### 3.3 syncJSONOutput mirrors SyncReport manually (three-way sync point)

**Report**: JSONOutputFormat

`sync.go` defines a separate `syncJSONOutput` struct that replicates every field of `sync.SyncReport` with JSON tags. Any new field added to `SyncReport` must also be manually added to `syncJSONOutput` and `syncReport` (the E2E mirror struct in `sync_helpers_test.go`). This is a four-copy maintenance burden (SyncReport, syncJSONOutput, syncReport test struct, and the JSON output format).

**Code locations**: `sync.go`: `syncJSONOutput`; `internal/sync/types.go`: `SyncReport`; `e2e/sync_helpers_test.go`: `syncReport`

---

### 3.4 DurationMs duplication in engine.go

**Report**: JSONOutputFormat

`RunOnce` computes `durationMs` inline at line 166 for logging, and separately `SyncReport.DurationMs()` performs the identical calculation. If the formula ever changes, both sites must be updated.

**Code location**: `engine.go:166`; `types.go:451`

---

### 3.5 flagVerbose and flagQuiet are package-level globals

**Report**: QuietMode

These are set as side effects of Cobra flag parsing (`root.go:19-26`). `buildLogger()` and `statusf()` implicitly depend on global state. If tests ever run in parallel within the same process, this would be a data race. Noted as known technical debt (B-036).

---

### 3.6 SyncMode.String() default returns "bidirectional" silently

**Report**: JSONOutputFormat

An unknown `SyncMode` value silently maps to `"bidirectional"` instead of an error indicator. If a new mode is added but `String()` is not updated, the JSON output would be misleading.

**Code location**: `types.go:206`

---

## 4. Decision Matrix Gaps

### 4.1 Missing D7 rule -- folder deletion not propagated remotely (CRITICAL)

**Report**: DeleteLocalFolder

The folder decision matrix (D1-D6) has no rule for: "folder missing locally, exists remotely, was synced, not tombstoned -> delete remotely." When a user deletes a folder locally:
- The file inside is correctly deleted remotely via F6.
- The folder itself hits D3 (`!localExists && remoteExists`) and is **re-created locally**, undoing the user's deletion.
- The folder remains on OneDrive as an empty folder.

The missing rule would be D7: `!localExists && remoteExists && synced && !tombstoned -> ActionRemoteDelete`.

**Code location**: `reconciler.go:294-323`: `dispatchFolder` -- handles 6 cases (D1-D6) but no local-deletion propagation

---

### 4.2 D1 structurally unreachable for folders

**Convergence**: 7+ reports (AlreadyInSync, DeleteLocalFile, IncrementalAddLocal, IncrementalEditLocal, IncrementalEditRemote, MultipleCycles, NewRemoteFolder)

D1 requires `localExists && remoteExists && synced`. For folders, `isSynced` always returns `false` because `SyncedHash` is never set (folders have no hash) and `LastSyncedAt` is never set by `executeSyncedUpdate`. D1 is dead code for folders -- they always go through D2 (adopt).

**Code location**: `reconciler.go:296`: D1 check; `reconciler.go:427-429`: `isSynced`

---

### 4.3 D4 structurally unreachable for folder tombstones

**Report**: DeleteRemoteFolder

When `MarkDeleted` tombstones a folder, `localExists` evaluates as `false` because the tombstoned row never has `LocalMtime` set (MarkDeleted only updates `is_deleted` and `deleted_at`). The scanner's `local:` row (which does have LocalMtime) is a separate DB row that goes to D5. The tombstoned folder row always has `localExists=false` and falls through to D6 (cleanup) or no-action, not D4.

**Code location**: `reconciler.go:276-286`: `newFolderState`; `delta.go`: `MarkDeleted`

---

### 4.4 F9 (edit-delete conflict) path unreachable

**Report**: EditDeleteConflict

See Section 1.1 (scanner tombstone blindness). The scanner splits state into two rows, and the tombstoned row's `LocalHash` is never updated, so `detectLocalChange` returns `false`. F9 is never reached; F8 fires instead, and the S4 safety check partially compensates but with incorrect report accounting.

---

### 4.5 F4 vs F10 coverage gap

**Report**: FalseConflict

`classifyBothChanged` has two "both changed, hashes match" branches: F4 (synced) and F10 (unsynced, create-create identical). The FalseConflict test only exercises F4. There is no E2E test for F10 (two clients simultaneously creating a file with identical content before either has been synced).

**Code location**: `reconciler.go:224-250`

---

### 4.6 F6/F7 ordering edge case for simultaneous local+remote deletion

**Report**: DeleteLocalFile

For simultaneous independent deletion (both `LocalHash==""` and `IsDeleted=true`), `classifyLocalDeletion` runs before `classifyRemoteTombstone` in `applyFileMatrix`. The code falls into F7 (re-download), which is a surprising outcome -- the clean result should be F14 (cleanup). This edge case is not exercised by any test.

**Code location**: `reconciler.go:160-168`: ordering of `classifyLocalDeletion` vs `classifyRemoteTombstone`

---

### 4.7 isSynced OR condition semantics

**Report**: FalseConflict, CreateCreateConflict

`isSynced` returns true if `SyncedHash != ""` OR `LastSyncedAt != nil`. A file with `SyncedHash=""` and a non-nil `LastSyncedAt` would be considered synced and could reach F4/F5, then fail the hash comparison unexpectedly. The dual condition creates subtle asymmetry with `detectLocalChange` which only checks `SyncedHash`.

**Code location**: `reconciler.go:427-429`

---

### 4.8 localExists in newFolderState includes dead code branch

**Report**: IncrementalAddLocal

`newFolderState` computes `localExists = item.LocalHash != "" || item.LocalMtime != nil`. For folders, `LocalHash` is never set, so the `LocalHash != ""` branch is dead code.

**Code location**: `reconciler.go:276-286`

---

## 5. Pipeline Ordering Issues

### 5.1 Upload phase runs before local_deletes phase

**Reports**: DeleteRemoteFile, DeleteRemoteFolder, EditDeleteConflict

The executor's 9-phase ordering places `uploads` (phase 4) before `local_deletes` (phase 5). When the scanner tombstone blindness bug (Section 1.1) produces both an `ActionUpload` and `ActionLocalDelete` for the same path, the file is re-uploaded before it is deleted locally. This makes the side effects of the two-row split worse.

**Code location**: `executor.go:100-171`: phase ordering

---

### 5.2 Delta runs before .nosync guard fires

**Report**: NosyncGuard

The `.nosync` guard check is in the scanner (Phase 2). The delta processor (Phase 1) runs first, writing 24 items and a delta token to the DB. If the `.nosync` guard fires, the DB already has side effects from the delta fetch. On the next sync (after `.nosync` is removed), the delta will use the persisted token.

**Code location**: `engine.go:126,131`: phase ordering; `scanner.go:90`: `checkNosyncGuard`

---

### 5.3 .nosync guard not covered by download-only mode

**Report**: NosyncGuard

In download-only mode, the local scan is skipped entirely (`engine.go:205`). Since the `.nosync` guard lives in the scanner, it is never checked in download-only mode.

---

### 5.4 D5 folder creation is sequential (no parallelism for siblings)

**Reports**: DeepNesting

`dispatchPhase` is strictly sequential. For wide directory trees (many siblings at the same depth), each create must wait for the previous one, even though siblings could theoretically be parallelized since they share the same parent ID.

**Code location**: `executor.go`: `dispatchPhase`

---

### 5.5 resolveParentID relies on same-phase DB state

**Report**: DeepNesting

The correctness of nested folder creation depends on `GetItemByPath` returning the freshly-upserted row from the immediately preceding `executeFolderCreate` call. This works because `dispatchPhase` is sequential, but it creates a subtle ordering dependency. If the phase were ever parallelized, sibling folders at the same depth would be safe, but children whose parents are in the same batch would break.

**Code location**: `executor.go:699-727`: `resolveParentID`

---

## 6. Data Model Problems

### 6.1 test-manual-delete.txt classified as folder

**Convergence**: 30+ reports observed this

The remote item `test-manual-delete.txt` has `ItemType=folder` in the DB and is processed as D3 (create folder locally). The `.txt` extension is misleading. This is a pre-existing artifact from manual testing on the test OneDrive account that creates a directory named `test-manual-delete.txt` locally.

---

### 6.2 subfolder/tesmi item type misclassification

**Convergence**: 30+ reports observed this

The item `subfolder/tesmi` is routed through `reconcileFile` (emitting `"classify file"` log messages) instead of `reconcileFolder`. This suggests its `ItemType` was stored as `ItemTypeFile` (or an unrecognized value) rather than `ItemTypeFolder`. The `convertGraphItem` function in `delta.go` may not correctly handle all folder facet patterns from the Graph API.

**Code location**: `delta.go`: `convertGraphItem`; `reconciler.go`: `reconcileItem` dispatch

---

### 6.3 Empty hashes for certain items

**Convergence**: Multiple reports

Items like `subfolder/tesmi` have empty hashes on all three sides (`localHash=""`, `remoteHash=""`, `syncedHash=""`). Zero-byte files, OneNote notebooks, or special OneDrive items may not have QuickXorHash values. The reconciler silently skips them via F1, but there is no WARN log when an item has no hash and is `synced=false` across multiple cycles.

---

### 6.4 local: key is filesystem-encoding-dependent on Linux

**Report**: UnicodeFilenames

The `local:` pseudo item ID embeds the NFC-normalized path directly. On Linux where NFD paths might exist, the `local:` key might not match if the OS returns an NFD path that normalizes differently. The `visited` map + `os.Stat` fallback handles this in orphan detection, but the key itself could mismatch.

**Code location**: `scanner.go`: `localItemID` construction

---

## 7. API Interaction Issues

### 7.1 HTTP timeout too short for large file uploads (CRITICAL)

**Report**: LargeFile

`httpClientTimeout = 30s` (`root.go:39`) is shared across all requests. For a 5 MiB upload, the 30-second window is consumed by TLS handshake + body upload + server processing. The upload timed out at exactly 30 seconds with `"context deadline exceeded (Client.Timeout exceeded while awaiting headers)"`.

**Code location**: `root.go:39`: `httpClientTimeout = 30 * time.Second`

---

### 7.2 ConnectTimeout / DataTimeout config fields are dead code

**Report**: LargeFile

`internal/config/config.go:96-97` defines `ConnectTimeout` and `DataTimeout`. `internal/config/defaults.go:106-107` sets defaults (10s/60s). `internal/config/validate.go:371-372` validates them. But no code in `root.go` or anywhere else reads them to configure the HTTP client. The configuration surface promises timeout control that is never delivered.

**Code locations**: `internal/config/config.go:96-97`, `internal/config/defaults.go:106-107`, `internal/config/validate.go:371-372`

---

### 7.3 No per-chunk retry in uploadChunked

**Report**: LargeFile

`executor.go:753-769` loops over chunks but has no retry. If a chunk fails, the upload session is abandoned (neither canceled nor resumed). `QueryUploadSession` and `CancelUploadSession` exist in `upload.go` but are never called on error.

**Code location**: `executor.go:753-769`; `upload.go`: unused `QueryUploadSession` / `CancelUploadSession`

---

### 7.4 UploadChunk bypasses retry logic

**Report**: LargeFile

`upload.go:147` calls `c.httpClient.Do(req)` directly instead of `c.Do()`. This skips the built-in retry/backoff for throttling and server errors. The decision is deliberate (session URLs are pre-authenticated), but it also skips retry.

**Code location**: `upload.go:147`

---

### 7.5 Single chunk covers entire file for files < chunkSize

**Report**: LargeFile

`chunkSize = 10 MiB` but the test file is 5 MiB. A single chunk covers the entire file. A timeout causes a complete loss with no partial progress to resume from. Smaller chunks would allow partial progress.

**Code location**: `executor.go`: `chunkSize = 10_485_760`

---

### 7.6 me/drives called on every sync invocation

**Report**: MultipleCycles

Each sync cycle issues `GET /me/drives` before opening the engine (~0.85s per call). At 4 cycles, ~3.4s is spent on drive discovery alone (15% of test duration). The drive ID could be cached in the state DB after the first successful discovery.

---

### 7.7 No upload-side hash verification

**Report**: UnicodeFilenames

The download path verifies `gotHash != action.Item.QuickXorHash` before renaming the partial file. The upload path computes a local hash via `io.TeeReader` but does not compare against the server's returned hash. Silent corruption during upload would not be caught until the next delta cycle.

---

### 7.8 Executor re-fetches item metadata before each download

**Report**: IncrementalEditRemote

The executor calls `graph.GetItem` to fetch fresh metadata before calling `graph.Download` -- two API round-trips per download. The metadata is needed for the download URL, but it adds latency.

---

### 7.9 WARN-level timestamps on deleted items

**Convergence**: DeleteRemoteFile, DeleteRemoteFolder, EditDeleteConflict

The Graph API omits `createdDateTime`/`lastModifiedDateTime` on tombstoned items. The normalizer correctly falls back to current time and logs at WARN. This warning appears consistently for every remote delete and could be reduced to DEBUG since it is a known, intentional fallback.

---

## 8. Test Quality Issues

### 8.1 Test isolation gap -- full drive sync (CRITICAL)

**Convergence**: Nearly every report (30+)

All E2E tests sync the entire OneDrive drive root, not just the test's remote folder. `newSyncEnv` configures `sync_dir` as a temp directory but does NOT set `sync_paths` to restrict syncing. The delta returns 24+ items including pre-existing folders (Documents, Pictures, Music, Apps, Recordings) and stale artifacts from prior test runs (`sync-e2e-*`, `onedrive-go-e2e-edge-*`, `manual-test/`).

Consequences:
- Every fresh-DB sync downloads unrelated files and creates 17+ folders
- SQLITE_BUSY contention is amplified by more concurrent downloads
- Assertions use `GreaterOrEqual` to tolerate noise, masking real failures
- Test runs are non-deterministic (depend on accumulated remote state)
- Test duration inflated by downloading unrelated data (~1.1 MB PDF)

**Fix**: Configure `sync_paths = ["/sync-e2e-<timestamp>"]` in `writeTestConfig`.

---

### 8.2 Weak test assertions (GreaterOrEqual instead of Equal)

**Convergence**: Nearly every report (20+)

Tests use `assert.GreaterOrEqual(t, report.Downloaded, 1)` instead of exact counts. This masks bugs:
- InitialDownload: 4 of 8 downloads succeeded; `GreaterOrEqual(3)` passes because 4 >= 3
- BigDeleteBlocked: `FoldersCreated >= 4` passes even if the specific 4 test folders failed
- BidirectionalMerge: `Downloaded >= 1` passes with 6 downloads (5 unrelated)
- DeepNesting: `FoldersCreated >= 4` passes from 17 pre-existing folder creates alone

---

### 8.3 Missing test assertions

Multiple reports identified specific missing assertions:

| Test | Missing Assertion | Report |
|------|-------------------|--------|
| DeleteRemoteFile | `report.Uploaded == 0` (re-upload bug undetected) | DeleteRemoteFile |
| DeleteRemoteFile | Remote file actually absent after sync | DeleteRemoteFile |
| DeleteRemoteFolder | Folder directory itself is absent locally | DeleteRemoteFolder |
| DeleteLocalFile | Local file still absent after sync | DeleteLocalFile |
| BigDeleteBlocked | Remote files still exist (no deletions performed) | BigDeleteBlocked |
| CreateCreateConflict | Content of conflict copy file | CreateCreateConflict |
| FalseConflict | `report.SyncedUpdates >= 1` | FalseConflict |
| JSONOutputFormat | `BytesUploaded`, `BytesDown`, `FoldersCreated`, `Downloaded` fields | JSONOutputFormat |
| DryRun/DryRunNoSideEffects | DB state post-dry-run | DryRun, DryRunNoSideEffects |
| MultipleCycles | Content verification for cycle 1 upload | MultipleCycles |
| BidirectionalMerge | Content of remote `from-local.txt` via `get` | BidirectionalMerge |
| BidirectionalMerge | `BytesDown` or `BytesUp` | BidirectionalMerge |

---

### 8.4 Remote test artifact accumulation

**Convergence**: Nearly every report

The `t.Cleanup` in `newSyncEnv` only removes the current test's remote folder. If a prior test panics or cleanup fails, folders accumulate at the drive root. Every subsequent sync test sees these as untracked remote items, inflating delta sizes and download counts.

---

### 8.5 runSyncJSON discards error and stderr

**Convergence**: Multiple reports (JSONOutputFormat, MultipleCycles, InitialDownload)

`runSyncJSON` (`sync_helpers_test.go:170`) calls `runSyncRaw` and discards the error return (`stdout, _, _ := env.runSyncRaw(...)`). A misconfigured test environment would produce an empty stdout and the JSON unmarshal would fail with a generic parse error rather than a clear diagnostic.

---

### 8.6 Test log duplication

**Convergence**: SpacesInFilenames, BigDeleteForce

The sync stderr is logged by the test helper at `t.Logf` and then also emitted inline in `t.Fatalf` error message. The log file appears to contain the entire stderr output twice.

**Code location**: `sync_helpers_test.go`: `runSync` calls `t.Logf(stderr)` then `t.Fatalf("...stderr: %s", stderr)`

---

### 8.7 syncReport test struct missing fields

**Reports**: DryRun, JSONOutputFormat

The `syncReport` struct in `sync_helpers_test.go` omits `synced_updates`, `cleanups`, and `skipped` fields. Tests using `runSyncJSON` cannot assert on these counts.

---

### 8.8 Test names misrepresent what is being tested

| Test Name | Reality | Report |
|-----------|---------|--------|
| SpacesInFilenames | Fails due to SQLITE_BUSY, not spaces | SpacesInFilenames |
| ExitCodeOnErrors | "Clean sync" is not clean (drive root has files) | ExitCodeOnErrors |
| DryRunNoSideEffects | DB has significant side effects | DryRunNoSideEffects |
| DeleteLocalFolder | Folder not actually deleted remotely | DeleteLocalFolder |

---

### 8.9 No second-sync idempotency verification

**Reports**: InitialUpload, InitialDownload, MultipleCycles

Most tests run a single sync and verify the result but never run a second sync to verify zero actions. The SQLITE_BUSY-induced state corruption (files on disk without DB records) would be caught by a second sync asserting `report.Uploaded == 0`.

---

### 8.10 deltaSleepDuration fragility

**Reports**: AlreadyInSync, BidirectionalMerge

The `deltaSleepDuration = 2 * time.Second` constant is used between remote writes and sync runs. If the delta API is slow to reflect newly uploaded files, tests become flaky. Some tests (AlreadyInSync) omit the sleep entirely between sync cycles and succeed only because the upload precedes the delta fetch by more than 2 seconds in practice.

---

## 9. Performance Concerns

### 9.1 Path materialization is O(depth) per item

**Convergence**: AlreadyInSync, InitialDownload, BidirectionalMerge, InitialUpload

During delta processing, each item's path is materialized by walking the parent chain via `store.GetItem`. For a 28-item batch, there are ~60 `getting item` debug log lines. The root item is fetched 15+ times. A simple in-memory path cache per flush batch would reduce this substantially.

**Code location**: `delta.go`: `materializePath`

---

### 9.2 17+ spurious synced_updates per cycle

**Convergence**: 7+ reports (see Section 1.4)

Every second sync produces 17+ DB writes for folder adoption (D2) that could be avoided if D3 or `executeSyncedUpdate` recorded the baseline on first creation.

---

### 9.3 WAL checkpoint runs even with zero actions

**Report**: MultipleCycles

In cycle 4 (pure idle), the executor runs all 9 phases (each producing a "phase starting" DEBUG log) and then runs the WAL checkpoint. A short-circuit before entering the 9-phase loop when `total_actions=0` would be slightly more efficient.

---

### 9.4 Sequential conflict resolution

**Reports**: EditEditConflict, CreateCreateConflict

Conflict resolution (rename + download) happens sequentially in the conflicts phase, not through the TransferManager worker pool. For a large number of conflicts with large files, this could be slow.

---

### 9.5 Double name-validation between filter and scanner

**Reports**: SkipDirsPattern, SkipDotfiles

`checkNameValidation` in filter and `isValidOneDriveName` in scanner both validate filenames. `maxPathChars` is defined in two places (`scanner.go:28` and `filter.go:17`).

---

### 9.6 detectLocalMoves overhead on large item sets

**Report**: BidirectionalMerge

`detectLocalMoves` in `reconciler.go:53` iterates all items to build hash maps. As the test account accumulates more artifacts, each test run processes a growing set of items it has no intention of syncing.

---

## 10. Logging / Observability Gaps

### 10.1 executeRemoteDelete does not log path

**Report**: DeleteLocalFile

`executor.go:612` logs `"executor: remote delete"` with `item_id` but not `path`. Every other executor action includes the `path` field. A user reading INFO-level logs cannot tell which filename was deleted without cross-referencing the item ID against the DB.

**Code location**: `executor.go:612`

---

### 10.2 executor: done omits delete counts

**Report**: DeleteLocalFile

The `executor: done` log line shows `downloaded=N uploaded=N errors=N` but does not include `remote_deleted` or `local_deleted`. These counts only appear in the subsequent engine-level log line.

---

### 10.3 No chunk progress logging for uploads

**Report**: LargeFile

The `uploading chunk` debug message logs `offset/length/total` but there is no per-second or per-percentage progress during the upload wait. For large files, this makes timeouts hard to diagnose in CI.

---

### 10.4 reorderDeletions is not logged

**Report**: DeleteRemoteFolder

The batch reorder (`delta.go:468-491`) is silent. There is no DEBUG log confirming the reorder fired or showing how many deletions were moved ahead of live items.

---

### 10.5 F1 / no-action decisions not logged

**Convergence**: AlreadyInSync, MultipleCycles, and many others

When the reconciler determines "no action needed" (F1 path), there is no debug log. Items like `subfolder/tesmi` silently fall through without any trace. A "no-op" decision should emit a debug log entry for traceability.

---

### 10.6 No scanner log for mtime fast-path exit

**Report**: IncrementalEditRemote

When the scanner detects that a file's mtime has not changed and skips re-hashing, there is no log entry. This makes it impossible to distinguish "scanner did not see the file" from "scanner saw the file but determined it was unchanged."

---

### 10.7 Missing log for local scan skip in download-only mode

**Report**: DownloadOnly

When `runScan` returns early in download-only mode, there is no debug log enumerating which local files existed but were not scanned. Diagnosing "why was my local file not uploaded?" post-hoc requires knowing that a prior download-only run left it invisible to the state store.

---

### 10.8 buildDryRunReport semantics ambiguous

**Reports**: DryRun, DryRunNoSideEffects

The dry-run report shows `Downloaded=5` even though no bytes were downloaded. The `SyncReport.Downloaded` field represents "planned" downloads in dry-run context but "completed" downloads in real-sync context. The `bytes_downloaded=0` field signals no transfer, but the counter ambiguity could confuse users.

---

### 10.9 No WARN for persistent zero-hash items

**Convergence**: 30+ reports (subfolder/tesmi)

An item with no hash on either side that persists across multiple cycles never triggers a WARN. The item is silently skipped via F1 every cycle without any diagnostic about why it has no hash.

---

### 10.10 FolderCreateSide logged as integer, not string

**Report**: UploadOnly

`FolderCreateSide` appears in logs as `side=1` or `side=2`. It would be more readable if it implemented `String()` (e.g., "local" / "remote").

---

### 10.11 S5 percentage uses integer division

**Report**: BigDeleteBlocked

`15 * 100 / 39 = 38` (integer truncation; actual is 38.46%). If an operator sets `big_delete_percentage = 38`, a deletion of 15/39 items would NOT trigger the check even though the true fraction exceeds 38%.

**Code location**: `safety.go`: `checkS5BigDelete`

---

### 10.12 Filter applied in scanner not reconciler (remote items unfiltered on download)

**Report**: SkipDotfiles

Filter rules (skip_dotfiles, skip_files, skip_dirs) are applied in the scanner phase, not the reconciler. Remote dotfiles arriving through the delta are not filtered and will be downloaded. Only local dotfiles are excluded from scanning.

**Code location**: `scanner.go`: filter integration; `reconciler.go`: no filter check

---

### 10.13 Symlink size check latent bug in filter ordering

**Report**: SkipFilesPattern

The filter runs before symlink resolution, so `info.Size()` returns the symlink's own size, not the target's. For size-based filtering, this could produce incorrect results.

**Code location**: `filter.go`: `ShouldSync` size check ordering
