# Sync Algorithm Specification: onedrive-go

This document specifies the synchronization algorithm for onedrive-go — the pipeline architecture, delta processing, local scanning, three-way merge reconciliation, conflict detection, safety checks, execution, and continuous mode operation. It is THE core document of the system.


---

## Table of Contents

1. [Overview](#1-overview)
2. [Pipeline Architecture](#2-pipeline-architecture)
3. [Delta Processing](#3-delta-processing)
4. [Local Filesystem Scanning](#4-local-filesystem-scanning)
5. [Reconciliation (Three-Way Merge)](#5-reconciliation-three-way-merge)
6. [Filtering](#6-filtering)
7. [Conflict Detection and Resolution](#7-conflict-detection-and-resolution)
8. [Safety Checks (Pre-Execution Gates)](#8-safety-checks-pre-execution-gates)
9. [Execution](#9-execution)
10. [Initial Sync (First Run)](#10-initial-sync-first-run)
11. [Continuous Mode (`--watch`)](#11-continuous-mode---watch)
12. [Crash Recovery](#12-crash-recovery)
13. [Sync Report](#13-sync-report)
14. [Verify Command](#14-verify-command)
- [Appendix A: Architecture Constraint Traceability](#appendix-a-architecture-constraint-traceability)
- [Appendix B: Design Differences](#appendix-b-design-differences)
- [Appendix C: Decision Log](#appendix-c-decision-log)

---

## 1. Overview

### 1.1 Purpose and Scope

This specification defines the complete synchronization algorithm for onedrive-go. It covers:

- How remote changes are fetched and normalized (delta processing)
- How local changes are detected (filesystem scanning)
- How local, remote, and previously-synced states are compared (three-way merge)
- How conflicts are detected and resolved
- How safety invariants protect user data
- How planned actions are executed via parallel worker pools
- How continuous mode operates with real-time change detection
- How the system recovers from crashes and interruptions

The algorithm is designed to be **safe first, correct second, fast third**. Every destructive operation has a guard. Every optimization must not compromise safety or correctness.

### 1.2 Key Definitions

| Term | Definition |
|------|-----------|
| **Sync cycle** | A single pass through the pipeline: fetch → scan → reconcile → execute |
| **Delta token** | Opaque cursor from Graph API that represents a point-in-time snapshot |
| **deltaLink** | URL returned on the last delta page containing the next delta token |
| **Synced base** | The snapshot of item state recorded after the last successful sync |
| **Three-way merge** | Comparison of current-local vs current-remote vs synced-base to detect changes |
| **Tombstone** | A soft-deleted item record retained for move detection (30-day default) |
| **Action plan** | Ordered list of typed actions produced by the reconciler for the executor |
| **Worker pool** | A set of goroutines processing a queue of jobs (downloads, uploads, or hash checks) |
| **Batch** | A group of items processed together (default 500), followed by a DB checkpoint |
| **Stale file** | A file excluded by a filter change but still present locally |
| **False conflict** | Both sides changed but arrived at identical content (resolved silently) |
| **Big delete** | A sync cycle that would delete more items than a safety threshold allows |

### 1.3 Safety Philosophy

The algorithm is designed around the principle that **data loss is the only unrecoverable failure**. Network errors, API quirks, bugs — all are recoverable. A deleted file that existed nowhere else is not.

Every design decision is filtered through the question: "If this fails halfway through, can the user recover?" If the answer is no, the design is wrong.

### 1.4 Safety Invariants

Seven absolute rules that the algorithm must never violate. Each is referenced throughout this document as **S1** through **S7**.

| ID | Invariant | Rationale |
|----|-----------|-----------|
| **S1** | Never delete a remote file based on local absence | A failed download must not cause remote deletion. Only items with confirmed prior local existence (synced_hash is set) can be treated as local deletions. Prevents the most catastrophic bug pattern. |
| **S2** | Never process deletions from incomplete enumeration | If a delta fetch fails mid-stream, process received items but never treat missing items as deletions. Incomplete data must not drive destructive operations. |
| **S3** | Atomic file writes | All downloads use: write to `.partial` file → verify hash → atomic rename to final path. A failed download never corrupts an existing file. |
| **S4** | Hash-before-delete guard | Before deleting a local file (due to remote deletion), verify that the local file's content hash matches `synced_hash`. If it differs, the user modified the file — back it up, do not delete. Note: `synced_hash` is always set to the server's response hash after transfers (see D17). For enriched files, `synced_hash` equals the enriched server hash, not the local hash. |
| **S5** | Big-delete protection | If a sync cycle would delete more than EITHER the absolute count threshold OR the percentage threshold, abort and require `--force`. Catches unmounted volumes, pattern matching errors, and API failures. |
| **S6** | Disk space check | Before each download, verify sufficient free disk space. Skip the download if free space would drop below `min_free_space` ([prd §11](prd.md)). |
| **S7** | Never upload temporary or partial files | Files matching `.partial`, `.tmp`, `~*`, and similar patterns are excluded by default. The scanner never picks up in-progress download artifacts. |

### 1.5 Sync Modes

All sync modes use the same pipeline but activate different stages.

| Mode | CLI Flag | Delta Fetch | Local Scan | Reconcile | Download | Upload | Local Delete | Remote Delete |
|------|----------|:-----------:|:----------:|:---------:|:--------:|:------:|:------------:|:-------------:|
| **Bidirectional** | (default) | Yes | Yes | Full matrix | Yes | Yes | Yes | Yes |
| **Download-only** | `--download-only` | Yes | No | Remote-only rows | Yes | No | With `--cleanup-local` | No |
| **Upload-only** | `--upload-only` | No | Yes | Local-only rows | No | Yes | No | With delete propagation |
| **Dry-run** | `--dry-run` | Yes | Yes | Full matrix | Preview | Preview | Preview | Preview |
| **One-shot** | (default) | Single | Single | Once | Once | Once | Once | Once |
| **Continuous** | `--watch` | On event | On event | Per event | Per event | Per event | Per event | Per event |

**Mode combinations**: `--download-only` and `--upload-only` are mutually exclusive. `--dry-run` combines with any mode. `--watch` combines with any direction mode.

---

## 2. Pipeline Architecture

### 2.1 Pipeline Diagram

```
┌─────────────────────────────────────────────────────────────────────┐
│                         Sync Engine                                  │
│                    (internal/sync/engine.go)                         │
└────────┬──────────────────────┬───────────────────────┬─────────────┘
         │                      │                       │
         ▼                      ▼                       │
┌─────────────────┐  ┌──────────────────┐              │
│  Delta Fetcher  │  │  Local Scanner   │              │
│  (delta.go)     │  │  (scanner.go)    │              │
│                 │  │                  │              │
│  Graph API      │  │  Filesystem      │              │
│  /delta         │  │  DFS walk        │              │
│  + Normalize    │  │  + Hash checks   │              │
└────────┬────────┘  └────────┬─────────┘              │
         │                     │                        │
         │  Updated remote     │  Updated local         │
         │  state in DB        │  state in DB            │
         │                     │                        │
         └──────────┬──────────┘                        │
                    │                                   │
                    ▼                                   │
         ┌──────────────────┐                          │
         │   Reconciler     │                          │
         │  (reconciler.go) │                          │
         │                  │                          │
         │  Three-way merge │                          │
         │  → Action plan   │                          │
         └────────┬─────────┘                          │
                  │                                    │
                  │  Action plan                       │
                  │                                    │
                  ▼                                    │
         ┌──────────────────┐                          │
         │  Safety Checks   │                          │
         │                  │                          │
         │  Big-delete gate │                          │
         │  Disk space gate │                          │
         └────────┬─────────┘                          │
                  │                                    │
                  ▼                                    ▼
         ┌──────────────────────────────────────────────┐
         │             Executor                         │
         │           (executor.go)                      │
         │                                              │
         │  ┌───────────┐ ┌──────────┐ ┌────────────┐  │
         │  │ Downloads │ │ Uploads  │ │  Checkers   │  │
         │  │ (pool: 8) │ │ (pool:8) │ │  (pool: 8) │  │
         │  └───────────┘ └──────────┘ └────────────┘  │
         │                                              │
         │  Folder ops → Moves → Transfers → Deletes   │
         └──────────────────────────────────────────────┘
```

### 2.2 Pipeline Stages by Sync Mode

| Stage | Bidirectional | Download-Only | Upload-Only | Dry-Run |
|-------|:------------:|:-------------:|:-----------:|:-------:|
| Delta Fetch | Run | Run | Skip | Run |
| Normalize | Run | Run | Skip | Run |
| Apply to DB (remote) | Run | Run | Skip | Run |
| Local Scan | Run | Skip | Run | Run |
| Apply to DB (local) | Run | Skip | Run | Run |
| Reconcile | Full matrix | Remote-only | Local-only | Full matrix |
| Safety Checks | Run | Run | Run | Skip (preview) |
| Execute | Run | Run | Run | Preview only |
| Verify & Update DB | Run | Run | Run | Skip |
| Save Checkpoint | Run | Run | Run | Skip |

### 2.3 Batch Processing Model

Delta items are processed in configurable batches (default 500 items per batch, per [architecture §6.5](architecture.md)):

1. Fetch one batch of delta items (up to 500 per page-group)
2. Normalize all items in the batch ([§3.2](#32-normalization))
3. Reorder deletions within the batch ([§3.3](#33-deletion-reordering))
4. Apply to state DB within a single transaction
5. Perform a WAL checkpoint to bound WAL file growth
6. Repeat until a `deltaLink` is received (all pages consumed)
7. Save the delta token from the `deltaLink` only after the complete response is processed

**Why batch?** Unbounded WAL growth during large initial enumerations (100K+ items) can consume excessive disk space. Batching with checkpoints prevents this while maintaining crash recovery at batch granularity.

### 2.4 Context Tree

One root context per sync run. Cancellation propagates to all stages (per [architecture §5.4](architecture.md)):

```
rootCtx
├── deltaFetchCtx
├── scannerCtx
├── reconcilerCtx
└── executorCtx
    ├── downloadWorker[0..N]
    ├── uploadWorker[0..N]
    └── checkWorker[0..N]
```

Bounded channels between pipeline stages provide backpressure. If the executor cannot keep up (all workers busy), the reconciler blocks, which in turn blocks the scanner ([architecture §5.3](architecture.md)).

---

## 3. Delta Processing

Delta processing fetches remote changes from the Graph API and applies them to the state database. This is the first stage of every sync cycle in bidirectional and download-only modes.

### 3.1 Delta Fetch Loop

```
function FetchAndApplyDeltas(ctx, driveID):
    token = store.GetDeltaToken(driveID)
    deltaComplete = false
    batch = []

    loop:
        // Fetch one page
        page, nextLink, deltaLink, err = api.DeltaPage(ctx, driveID, token)

        if err is HTTP410:
            return HandleResync(ctx, driveID, err.ResyncType)

        if err is retryable:
            retry with backoff (max 5 attempts)
            continue

        if err is fatal:
            return err  // Do not process partial results as deletions

        // Normalize all items on this page
        normalized = normalize.NormalizeDeltaPage(page)

        // Accumulate into batch
        batch = append(batch, normalized...)

        // Process batch when it reaches threshold
        if len(batch) >= BATCH_SIZE:
            ReorderDeletions(batch)
            ApplyBatchToDB(ctx, driveID, batch)
            store.WALCheckpoint()
            batch = []

        if deltaLink != "":
            // Last page reached — complete response
            deltaComplete = true
            newToken = extractToken(deltaLink)
            break
        else:
            token = nextLink  // Follow pagination

    // Process any remaining items
    if len(batch) > 0:
        ReorderDeletions(batch)
        ApplyBatchToDB(ctx, driveID, batch)
        store.WALCheckpoint()

    // Save token ONLY after complete response
    if deltaComplete:
        store.SaveDeltaToken(driveID, newToken)
        store.SetDeltaComplete(driveID, true)
    else:
        store.SetDeltaComplete(driveID, false)

    return nil
```

**Key property**: The delta token is saved only after the complete response (all pages through `deltaLink`) has been processed. If the process crashes mid-fetch, the next run re-fetches from the last saved token, reprocessing at most one batch of already-applied items (which is idempotent because of upsert semantics).

### 3.2 Normalization

Every item from the Graph API passes through the normalize layer ([architecture §8](architecture.md)) before being processed. Normalization is applied per-page to the entire batch.

| Quirk | Normalization |
|-------|--------------|
| driveId casing | `strings.ToLower()` on `parentReference.driveId` |
| driveId truncation | Left-pad Personal driveIds with `0` to 16 characters |
| Missing `name` (Business deleted) | Look up from state DB by `(driveId, itemId)` |
| Missing `size` (Personal deleted) | Look up from state DB by `(driveId, itemId)` |
| Bogus hash on deleted items | Set `quickXorHash` to `nil`; ignore server hash |
| Invalid/missing timestamps | Validate range; fall back to `NowNano()` |
| Fractional seconds | Truncate `fileSystemInfo` timestamps to whole seconds for comparison |
| URL-encoded paths | URL-decode `parentReference.path` when present |
| Duplicate items in page | Keep last occurrence per `(driveId, itemId)` |
| OneNote packages | Detect via `package` facet; mark as skip |
| Items with `malware` facet | Mark as skip; never download |

```
function NormalizeDeltaPage(page []DriveItem) []NormalizedItem:
    // Deduplicate: keep last occurrence per (driveId, itemId)
    seen = map[string]*NormalizedItem{}

    for item in page:
        n = NormalizedItem{}
        n.DriveID = normalizeDriveID(item.ParentReference.DriveID)
        n.ItemID = item.ID
        n.ParentDriveID = normalizeDriveID(item.ParentReference.DriveID)
        n.ParentID = item.ParentReference.ID
        n.Name = item.Name
        n.ItemType = classifyItem(item)  // file, folder, root, remote, package

        // Handle missing fields on deleted items
        if item.Deleted != nil:
            n.IsDeleted = true
            n.QuickXorHash = ""  // Ignore bogus hash
            if n.Name == "":
                dbItem = store.GetItem(n.DriveID, n.ItemID)
                if dbItem != nil:
                    n.Name = dbItem.Name
            if item.Size == 0 and n.ItemType == "file":
                dbItem = store.GetItem(n.DriveID, n.ItemID)
                if dbItem != nil:
                    n.Size = dbItem.Size
        else:
            n.QuickXorHash = item.File.Hashes.QuickXorHash
            n.Size = item.Size

        // Timestamp normalization
        n.RemoteMtime = parseAndValidateTimestamp(item.FileSystemInfo.LastModifiedDateTime)

        // Skip OneNote packages
        if item.Package != nil and item.Package.Type == "oneNote":
            n.Skip = true
            n.SkipReason = "OneNote package"

        // Skip malware-flagged items
        if item.Malware != nil:
            n.Skip = true
            n.SkipReason = "malware detected"

        key = n.DriveID + ":" + n.ItemID
        seen[key] = n

    return values(seen)
```

### 3.3 Deletion Reordering

The Graph API can deliver deletions AFTER creations at the same path within a single delta page. This ordering bug means that if the sync engine processes items in received order, it may create a file and then immediately delete it — losing data.

**Solution**: Buffer the full normalized page, partition into deleted and non-deleted items, and process deletions first.

```
function ReorderDeletions(batch []NormalizedItem):
    deleted = []
    nonDeleted = []

    for item in batch:
        if item.IsDeleted:
            deleted = append(deleted, item)
        else:
            nonDeleted = append(nonDeleted, item)

    // Rebuild batch: deletions first, then creates/updates
    batch = append(deleted, nonDeleted...)
```

**Why page-level, not cross-page?** The Graph API guarantees ordering within a page for parent-child relationships (parents before children for creations). Reordering across pages could break this guarantee. The deletion reorder only swaps deletions ahead of creations within the same buffered batch.

### 3.4 Applying Delta Items to State DB

Each normalized item is applied to the state database using a decision tree:

```
function ApplyDeltaItem(ctx, item NormalizedItem):
    if item.Skip:
        return  // OneNote, malware, etc.

    existing = store.GetItem(item.DriveID, item.ItemID)

    if item.IsDeleted:
        if existing == nil:
            return  // Already gone or never tracked
        if existing.IsDeleted:
            return  // Already tombstoned

        // Mark as tombstone (do not hard-delete)
        store.MarkDeleted(item.DriveID, item.ItemID, NowNano())
        return

    if existing == nil:
        // New item from remote
        newItem = createItemFromNormalized(item)
        newItem.Path = store.MaterializePath(item.DriveID, item.ItemID)
        store.UpsertItem(newItem)
        return

    if existing.IsDeleted:
        // Resurrection: previously tombstoned item reappears
        // This is a move detected via tombstone
        existing.IsDeleted = false
        existing.DeletedAt = nil
        updateRemoteFields(existing, item)
        existing.Path = store.MaterializePath(item.DriveID, item.ItemID)
        store.UpsertItem(existing)
        return

    // Existing live item — update remote fields
    oldParentID = existing.ParentID
    oldName = existing.Name
    updateRemoteFields(existing, item)

    // Detect and handle path changes (move/rename)
    if item.ParentID != oldParentID or item.Name != oldName:
        oldPath = existing.Path
        existing.Path = store.MaterializePath(item.DriveID, item.ItemID)
        if existing.ItemType == "folder" and oldPath != existing.Path:
            store.CascadePathUpdate(oldPath, existing.Path)

    store.UpsertItem(existing)
```

### 3.5 HTTP 410 Handling (Delta Token Expiration)

When the Graph API returns HTTP 410 Gone, the delta token has expired. The response includes a resync type hint that tells the client how to recover.

| Resync Type | Meaning | Our Handling |
|-------------|---------|-------------|
| `resyncChangesApplyDifferences` | Server state is authoritative. Client should apply server state and upload local unknowns. | Re-enumerate from scratch. Apply all server items. Then scan local for items not on server and upload them. |
| `resyncChangesUploadDifferences` | Client should upload items the server did not return. | Re-enumerate from scratch. Apply all server items. Then scan local for items not on server and upload them. |

**Design note**: We differentiate between the two resync types to handle the edge case correctly, but in practice both types result in the same action sequence because our three-way merge naturally handles the distinction.

```
function HandleResync(ctx, driveID, resyncType):
    log.Warn("delta token expired, performing full re-enumeration",
             "driveID", driveID, "resyncType", resyncType)

    // Delete the expired token
    store.DeleteDeltaToken(driveID)
    store.SetDeltaComplete(driveID, false)

    // Perform full enumeration (same as initial sync)
    FetchAndApplyDeltas(ctx, driveID)  // No token → full enumeration
```

### 3.6 Delta Completeness Tracking

A boolean `deltaComplete` flag per drive tracks whether the most recent delta fetch completed successfully (received a `deltaLink`). This flag is critical for safety invariant **S2**:

- **`deltaComplete = true`**: The remote view in the state DB is complete. Deletions (items in DB but not in remote) can be processed.
- **`deltaComplete = false`**: The remote view is incomplete (fetch was interrupted). Deletions MUST NOT be processed. Only additions and modifications are safe.

The reconciler checks this flag before generating any deletion actions from the remote side.

---

## 4. Local Filesystem Scanning

Local scanning detects changes on the local filesystem by walking the sync directory and comparing against the state database. This is the second stage of every sync cycle in bidirectional and upload-only modes.

### 4.1 Scanner Algorithm

```
function ScanLocalFilesystem(ctx, syncRoot, filterEngine):
    // Walk the filesystem using depth-first traversal
    walkDir(ctx, syncRoot, "", filterEngine)

function walkDir(ctx, fsRoot, relPath, filterEngine):
    fullPath = join(fsRoot, relPath)
    entries = readDir(fullPath)

    for entry in entries:
        if ctx.Done():
            return

        entryRelPath = join(relPath, entry.Name)
        entryFullPath = join(fsRoot, entryRelPath)

        // Step 1: Filter cascade
        included, reason = filterEngine.ShouldSync(entryRelPath, entry.IsDir, entry.Size)
        if not included:
            log.Debug("excluded by filter", "path", entryRelPath, "reason", reason)
            continue

        // Step 2: Name validation
        if not isValidOneDriveName(entry.Name):
            log.Warn("invalid OneDrive name, skipping", "path", entryRelPath)
            continue

        // Step 3: UTF-8 validation
        if not utf8.Valid(entry.Name):
            log.Warn("invalid UTF-8 filename, skipping", "path", entryRelPath)
            continue

        // Step 4: Symlink handling
        if entry.IsSymlink:
            if config.SkipSymlinks:
                continue
            target, err = resolveSymlink(entryFullPath)
            if err != nil:
                log.Warn("broken symlink, skipping", "path", entryRelPath)
                continue
            if isCircular(target, visited):
                log.Warn("circular symlink, skipping", "path", entryRelPath)
                continue
            entry = statTarget(target)

        if entry.IsDir:
            // Recurse into directory
            walkDir(ctx, fsRoot, entryRelPath, filterEngine)
        else:
            // Process file
            processLocalFile(ctx, entryFullPath, entryRelPath, entry)

function processLocalFile(ctx, fullPath, relPath, entry):
    existingItem = store.GetItemByPath(relPath)

    if existingItem == nil:
        // New local file — not in DB at all
        hash = computeQuickXorHash(fullPath)  // Via checker pool
        store.UpsertItem(Item{
            Path:       relPath,
            LocalSize:  entry.Size,
            LocalMtime: toUnixNano(entry.ModTime),
            LocalHash:  hash,
            ItemType:   "file",
            Name:       entry.Name,
            CreatedAt:  NowNano(),
            UpdatedAt:  NowNano(),
        })
        return

    if existingItem.IsDeleted:
        // Item was tombstoned but file reappeared locally
        // Could be a new file at same path, or user restored it
        hash = computeQuickXorHash(fullPath)
        existingItem.LocalSize = entry.Size
        existingItem.LocalMtime = toUnixNano(entry.ModTime)
        existingItem.LocalHash = hash
        existingItem.IsDeleted = false
        existingItem.DeletedAt = nil
        existingItem.UpdatedAt = NowNano()
        store.UpsertItem(existingItem)
        return

    // Existing live item — check for changes using fast path
    localMtime = toUnixNano(entry.ModTime)

    // Fast path: if mtime matches, skip hash computation
    if truncateToSeconds(localMtime) == truncateToSeconds(existingItem.LocalMtime):
        // No change detected via timestamp
        return

    // Slow path: mtime changed, compute hash to detect real content change
    hash = computeQuickXorHash(fullPath)  // Via checker pool

    existingItem.LocalSize = entry.Size
    existingItem.LocalMtime = localMtime
    existingItem.LocalHash = hash
    existingItem.UpdatedAt = NowNano()
    store.UpsertItem(existingItem)
```

### 4.2 Timestamp Comparison Rules

Timestamps are unreliable across filesystems and APIs. The scanner uses these rules:

1. **Truncate to whole seconds** before comparison. OneDrive does not store fractional seconds. Different filesystems have different precision (ext4: 1ns, FAT32: 2s, NTFS: 100ns). Truncating both sides to whole seconds prevents false positives.

2. **UTC normalization**. All local timestamps are converted to UTC via `time.Time.UTC()` before conversion to nanoseconds. All API timestamps are already UTC.

3. **Fast path (mtime match → skip hash)**. If the truncated local mtime matches the stored `local_mtime` in the database, the file is assumed unchanged. No hash is computed. This is critical for performance with large file trees.

4. **Slow path (mtime differs → hash)**. If mtime changed, compute the QuickXorHash. If the hash matches the stored `local_hash`, only the timestamp drifted (e.g., a file indexer touched the file). Update the timestamp in the DB but do not flag as changed for reconciliation.

### 4.3 Orphan Detection (Local Deletions)

Detecting local deletions (files that existed at last sync but are now gone) requires care to satisfy safety invariant **S1**.

```
function DetectLocalDeletions(ctx, syncRoot):
    // Query all non-deleted items that have been synced at least once
    syncedItems = store.ListSyncedItems()  // WHERE synced_hash IS NOT NULL
                                           //   AND is_deleted = 0

    for item in syncedItems:
        if ctx.Done():
            return

        fullPath = join(syncRoot, item.Path)

        if fileExists(fullPath):
            continue  // Still present, not a local deletion

        // File is missing locally. Was it previously confirmed synced?
        if item.SyncedHash == "" or item.LastSyncedAt == 0:
            // Never successfully synced — could be a failed download (S1)
            // Do NOT treat as local deletion
            log.Debug("unsynced item missing locally, ignoring",
                      "path", item.Path)
            continue

        // Confirmed previously synced and now missing → local deletion
        item.LocalHash = ""
        item.LocalSize = nil
        item.LocalMtime = nil
        item.UpdatedAt = NowNano()
        store.UpsertItem(item)
```

**Safety invariant S1 enforcement**: Only items with a non-null `synced_hash` (meaning they were previously confirmed to exist locally after a successful sync) can be treated as local deletions. Items that were never synced (e.g., a download failed, or they are new remote items not yet downloaded) are excluded from deletion detection.

### 4.4 Scanner Performance

The scanner runs in its own goroutine and uses the checker worker pool for parallel hash computation:

- **Checker pool** (default 8 workers): Hash jobs are submitted to a buffered channel. Each checker goroutine reads a file and computes its QuickXorHash using `io.TeeReader` for streaming computation.
- **File stat is cheap**: The `readDir` + `stat` calls that drive the walk are fast. The bottleneck is hash computation for changed files.
- **Fast-path ratio**: In steady state (few changes), the vast majority of files match on mtime and skip hashing entirely. Only files with changed mtimes enter the slow path.

### 4.5 Local Directory Handling

Directories are tracked in the state DB for move detection and path materialization, but they do not have content hashes. The scanner:

1. Creates DB entries for new local directories (discovered during walk, not in DB)
2. Detects missing local directories (in DB but not on filesystem) for orphan detection
3. Does NOT compute hashes for directories — folder reconciliation is existence-based ([§5.3](#53-folder-reconciliation))

---

## 5. Reconciliation (Three-Way Merge)

The reconciler is the heart of the sync algorithm. It reads the three states (local current, remote current, synced base) from each item in the database and produces an action plan.

### 5.1 Three-Way Merge Algorithm

For each non-deleted item in the state database, the reconciler computes two signals:

```
function Reconcile(ctx, mode SyncMode) ActionPlan:
    plan = ActionPlan{}
    items = store.ListAllActiveItems()  // is_deleted = 0

    for item in items:
        actions = reconcileItem(item, mode)
        plan.Append(actions...)

    // Add folder operations (creates, deletes) with ordering
    plan.OrderFolderOps()

    return plan

function reconcileItem(item, mode) []Action:
    localChanged = detectLocalChange(item)
    remoteChanged = detectRemoteChange(item)

    // Apply mode-specific filtering
    if mode == DownloadOnly:
        localChanged = false  // Ignore local changes
    if mode == UploadOnly:
        remoteChanged = false  // Ignore remote changes

    return applyDecisionMatrix(item, localChanged, remoteChanged)
```

**Change detection** ([data-model §12](data-model.md)):

The merge uses **per-side baselines** for change detection: local changes are compared against the last known local hash (`LocalHash`), and remote changes are compared against the last known remote hash (`QuickXorHash`). This is critical for handling SharePoint enrichment, where the server modifies uploaded files so that local and remote hashes legitimately diverge. A single shared baseline (`SyncedHash`) cannot satisfy both comparisons when local content differs from remote content. See [SHAREPOINT_ENRICHMENT.md](../../SHAREPOINT_ENRICHMENT.md) for the full rationale and edge case analysis.

```
function detectLocalChange(item) bool:
    // No synced base → item is new
    if item.SyncedHash == nil:
        // New item — local_hash presence indicates local-created
        return item.LocalHash != ""

    // Local columns cleared → locally deleted
    if item.LocalHash == "":
        return true

    // Content comparison against last known LOCAL state (per-side baseline)
    return item.LocalHash != item.LocalHash_baseline
        or item.LocalSize != item.LocalSize_baseline
    // Where LocalHash_baseline and LocalSize_baseline are the LocalHash
    // and LocalSize values recorded at last sync (i.e., the stored item fields).
    // In practice: currentLocalHash != item.LocalHash

function detectRemoteChange(item) bool:
    // No synced base → item is new
    if item.SyncedHash == nil:
        // New item — quick_xor_hash presence indicates remote-created
        return item.QuickXorHash != ""

    // Item tombstoned → remotely deleted
    if item.IsDeleted:
        return true

    // Content comparison against last known REMOTE state (per-side baseline)
    return currentRemoteHash != item.QuickXorHash
        or item.RemoteMtime != item.SyncedMtime
```

To be precise, the reconciler computes change detection as:

```
remoteChanged = (currentRemoteHash != item.QuickXorHash)  // last known remote hash
localChanged  = (currentLocalHash  != item.LocalHash)      // last known local hash
```

This replaces the previous single-baseline approach (`!= item.SyncedHash` for both sides). The per-side baselines ensure that after a SharePoint upload where enrichment produces divergent hashes (local=AAA, remote=BBB), the next cycle detects no change on either side — preventing infinite loops without downloading the enriched version. See [SHAREPOINT_ENRICHMENT.md §4.1](../../SHAREPOINT_ENRICHMENT.md) for details.

### 5.2 Decision Matrix

The complete decision matrix covers every combination of local and remote states. Each row maps to a specific action or set of actions.

#### File Decision Matrix

| # | Local State | Remote State | Synced Base | Hash Match? | Action | Safety |
|---|-------------|-------------|-------------|:-----------:|--------|--------|
| F1 | Unchanged | Unchanged | Exists | - | **No action** (in sync) | - |
| F2 | Unchanged | Changed | Exists | - | **Download**: pull remote to local | S3, S6 |
| F3 | Changed | Unchanged | Exists | - | **Upload**: push local to remote | - |
| F4 | Changed | Changed | Exists | Yes | **False conflict**: update synced base | - |
| F5 | Changed | Changed | Exists | No | **Conflict**: record + resolve per policy | S4 |
| F6 | Absent (deleted) | Unchanged | Exists | - | **Remote delete**: propagate local deletion | S1, S4, S5 |
| F7 | Absent (deleted) | Changed | Exists | - | **Download**: remote wins, re-download | S3, S6 |
| F8 | Unchanged | Absent (tombstoned) | Exists | - | **Local delete**: apply remote deletion | S2, S4, S5 |
| F9 | Changed | Absent (tombstoned) | Exists | - | **Edit-delete conflict**: local modified, remote deleted | S4 |
| F10 | Present | Present | None (new) | Yes | **False conflict**: both created identical file | - |
| F11 | Present | Present | None (new) | No | **Create-create conflict**: both created different file | S4 |
| F12 | Present | Absent | None (new) | - | **Upload**: new local file | - |
| F13 | Absent | Present | None (new) | - | **Download**: new remote file | S3, S6 |
| F14 | Absent | Absent | Exists | - | **Cleanup**: both sides deleted, purge DB record | - |

**Detailed row definitions**:

- **F1 (No action)**: `localChanged=false, remoteChanged=false`. Both sides match the synced base. Nothing to do.
- **F2 (Download)**: `localChanged=false, remoteChanged=true`. Only the remote changed. Download the new remote version, update local state.
- **F3 (Upload)**: `localChanged=true, remoteChanged=false`. Only the local changed. Upload the new local version, update remote state.
- **F4 (False conflict)**: `localChanged=true, remoteChanged=true, local_hash == quick_xor_hash`. Both sides changed but converged to the same content. Update synced base to match both. No transfer needed.
- **F5 (Conflict)**: `localChanged=true, remoteChanged=true, local_hash != quick_xor_hash`. True conflict. Apply resolution policy ([§7](#7-conflict-detection-and-resolution)).
- **F6 (Remote delete)**: Local file is gone, remote unchanged. Propagate the local deletion to remote. Subject to big-delete protection (S5) and hash-before-delete (S4 via S1 — only if previously synced).
- **F7 (Re-download)**: Local file is gone, but remote was modified. Remote wins — download the new version. The local deletion is overridden.
- **F8 (Local delete)**: Remote was deleted (tombstoned), local unchanged. Delete the local file. Subject to hash-before-delete (S4) and incomplete-delta guard (S2).
- **F9 (Edit-delete conflict)**: Remote was deleted but local was modified. This is a conflict — the user edited a file that was deleted remotely. Resolve per conflict policy.
- **F10 (Identical new)**: Both sides created the same file (same hash). Just update synced base.
- **F11 (Create-create conflict)**: Both sides created a different file at the same path. Resolve per conflict policy.
- **F12 (New local)**: File exists locally but not remotely, and has no synced base. Upload it.
- **F13 (New remote)**: File exists remotely but not locally, and has no synced base. Download it.
- **F14 (Both deleted)**: Both sides deleted. Clean up the DB record (convert tombstone to hard delete or let tombstone age out).

#### Folder Decision Matrix

Folders use existence-based reconciliation (no content hashing):

| # | Local State | Remote State | Synced Base | Action |
|---|-------------|-------------|-------------|--------|
| D1 | Exists | Exists | Exists | **No action** |
| D2 | Exists | Exists | None | **Adopt**: record in DB |
| D3 | Missing | Exists | Any | **Create locally**: mkdir |
| D4 | Exists | Missing (tombstoned) | Exists | **Delete locally**: rmdir (only if empty after all other ops) |
| D5 | Exists | Missing | None | **Create remotely**: mkdir on OneDrive |
| D6 | Missing | Missing | Exists | **Cleanup**: purge DB record |
| D7 | Missing | Exists (moved) | Exists | **Move locally**: rename/move local dir |

### 5.3 Folder Reconciliation

Folder operations have ordering constraints that file operations do not:

1. **Creates before children**: Parent folders must exist before any files within them are transferred. The action plan orders folder creates in top-down (depth-first) order.
2. **Deletes after children**: A folder can only be deleted after all its contents have been processed. The action plan orders folder deletes in bottom-up (deepest-first) order.
3. **Moves before children**: If a folder is moved/renamed, its children's paths change. The move must be applied before any child operations.

```
function OrderFolderOps(plan):
    // Sort folder creates by depth (shallowest first)
    sort(plan.FolderCreates, byPathDepthAscending)

    // Sort folder deletes by depth (deepest first)
    sort(plan.FolderDeletes, byPathDepthDescending)

    // Execution order:
    // 1. Folder creates (top-down)
    // 2. Moves/renames
    // 3. File transfers (parallel)
    // 4. File deletes
    // 5. Folder deletes (bottom-up)
```

### 5.4 Move/Rename Detection

Moves and renames are detected through two mechanisms:

#### Remote Moves (Tombstone-Based)

When the delta processor encounters an item whose `(driveId, itemId)` already exists in the state DB but under a different `parentId` or with a different `name`:

```
function detectRemoteMove(item, existing) MoveAction:
    if existing == nil:
        return nil  // Not a move

    if item.ParentID == existing.ParentID and item.Name == existing.Name:
        return nil  // Same location

    // Same item ID, different location → move or rename
    return MoveAction{
        DriveID:     item.DriveID,
        ItemID:      item.ItemID,
        OldPath:     existing.Path,
        NewPath:     computeNewPath(item),
        IsRename:    item.ParentID == existing.ParentID,  // Same parent = rename
        IsMove:      item.ParentID != existing.ParentID,  // Different parent = move
    }
```

If an item ID was previously tombstoned (deleted) and then reappears at a new location in a subsequent delta, the tombstone enables recognition as a move rather than a delete + create.

#### Local Moves (Hash-Based)

Local moves are harder to detect because the filesystem does not provide stable item IDs. When a local file disappears from one path and a file with the same hash appears at another path (within the same sync cycle), it is a candidate for local move detection:

```
function detectLocalMoves(deletedLocally, newLocally) []MoveAction:
    moves = []

    // Build a map of new local files by hash
    newByHash = map[string][]Item{}
    for item in newLocally:
        if item.LocalHash != "":
            newByHash[item.LocalHash] = append(newByHash[item.LocalHash], item)

    // For each locally deleted file, look for a hash match in new files
    for deleted in deletedLocally:
        if deleted.SyncedHash == "":
            continue
        candidates = newByHash[deleted.SyncedHash]
        if len(candidates) == 1:
            // Unique hash match → move
            moves = append(moves, MoveAction{
                DriveID:  deleted.DriveID,
                ItemID:   deleted.ItemID,
                OldPath:  deleted.Path,
                NewPath:  candidates[0].Path,
                IsLocal:  true,
            })
            // Remove from new list to avoid double-processing
            delete(newByHash, deleted.SyncedHash)
        // Multiple candidates: ambiguous, treat as delete + create
    return moves
```

**Why unique-match only?** If multiple new files have the same hash, it is ambiguous which one is the "moved" file. In that case, we fall back to delete + create, which is safe (if slightly slower).

### 5.5 Action Plan Generation

The reconciler produces a typed action plan:

```
type ActionType int

const (
    ActionDownload      ActionType = iota  // Pull remote file to local
    ActionUpload                           // Push local file to remote
    ActionLocalDelete                      // Delete local file/folder
    ActionRemoteDelete                     // Delete remote file/folder
    ActionLocalMove                        // Rename/move local file/folder
    ActionRemoteMove                       // Rename/move remote file/folder
    ActionFolderCreate                     // Create folder (local or remote)
    ActionConflict                         // Record and resolve conflict
    ActionUpdateSynced                     // Update synced base (false conflict)
    ActionCleanup                          // Remove stale DB record
)

type Action struct {
    Type       ActionType
    DriveID    string
    ItemID     string
    Path       string
    NewPath    string           // For moves
    Item       *Item            // Full item state
    ConflictInfo *ConflictInfo  // For conflict actions
}

type ActionPlan struct {
    FolderCreates  []Action  // Ordered top-down by depth
    Moves          []Action  // Folder moves first, then file moves
    Downloads      []Action  // Parallel execution
    Uploads        []Action  // Parallel execution
    LocalDeletes   []Action  // Files first, then folders bottom-up
    RemoteDeletes  []Action  // Files first, then folders bottom-up
    Conflicts      []Action  // Recorded and resolved per policy
    SyncedUpdates  []Action  // False conflicts and bookkeeping
    Cleanups       []Action  // DB record cleanup
}
```

### 5.6 Mode-Specific Reconciliation

Each sync mode activates a subset of the decision matrix:

| Mode | Active Rows (Files) | Active Rows (Folders) |
|------|--------------------|-----------------------|
| **Bidirectional** | F1-F14 | D1-D7 |
| **Download-only** | F1, F2, F8, F13, F14 | D1, D3, D4 (with `--cleanup-local`), D6 |
| **Upload-only** | F1, F3, F6, F12, F14 | D1, D5, D6 |
| **Dry-run** | All rows, but actions are preview-only | All rows, preview-only |

In **download-only** mode:
- Local changes are ignored (`localChanged` is always `false`)
- Remote deletions propagate locally only with `--cleanup-local`
- No uploads, no remote deletes

In **upload-only** mode:
- Remote changes are ignored (`remoteChanged` is always `false`)
- Local deletions propagate remotely unless `--no-remote-delete` is set
- No downloads, no local deletes

---

## 6. Filtering

Filtering determines which items are included in the sync scope. Filters are evaluated at two points in the pipeline: during delta processing (remote items) and during local scanning (local items).

### 6.1 Three-Layer Cascade

Each layer can only exclude more; never include back. Evaluation order matches [architecture §11](architecture.md):

```
Item path
  │
  ▼
┌─────────────────────┐
│ 1. sync_paths       │  If set, only these paths considered.
│    allowlist         │  Everything else excluded immediately.
│                     │  Exception: parent dirs of allowed paths
│                     │  are traversed (but not synced).
└─────────┬───────────┘
          │ (passes)
          ▼
┌─────────────────────┐
│ 2. Config patterns  │  skip_files, skip_dirs, skip_dotfiles,
│                     │  max_file_size
└─────────┬───────────┘
          │ (passes)
          ▼
┌─────────────────────┐
│ 3. .odignore        │  Per-directory marker files with
│    marker files     │  gitignore-style patterns
└─────────┬───────────┘
          │ (passes)
          ▼
      INCLUDED
```

The filter engine is pure logic with no I/O ([architecture §3.4](architecture.md)). It is initialized once from the config and `.odignore` files, and provides a single evaluation function:

```
function ShouldSync(path string, isDir bool, size int64) (bool, string):
    // Layer 1: sync_paths
    if syncPaths is configured:
        if not matchesSyncPaths(path, isDir):
            return false, "not in sync_paths"

    // Layer 2: Config patterns
    if isDir:
        if matchesSkipDirs(path):
            return false, "matches skip_dirs pattern"
        if config.SkipDotfiles and basename(path) starts with ".":
            return false, "dotfile excluded"
    else:
        if matchesSkipFiles(path):
            return false, "matches skip_files pattern"
        if config.SkipDotfiles and basename(path) starts with ".":
            return false, "dotfile excluded"
        if config.MaxFileSize > 0 and size > config.MaxFileSize:
            return false, "exceeds max_file_size"

    // Layer 3: .odignore
    if matchesOdignore(path, isDir):
        return false, "excluded by .odignore"

    return true, ""
```

### 6.2 Filter Evaluation Points

| Pipeline Stage | What is Filtered | How |
|---------------|-----------------|-----|
| **Delta processing** | Remote items from API | Each normalized item is evaluated before applying to state DB. Excluded items are skipped entirely. |
| **Local scanning** | Local filesystem entries | Each discovered entry is evaluated before processing. Excluded entries are skipped (directories are not recursed into). |
| **Reconciliation** | Items in state DB | Items that were previously synced but are now excluded by a filter change are detected via config snapshot comparison ([§6.3](#63-filter-changes-and-stale-files)). |

**Cascading exclusion**: If a directory is excluded, all its descendants are automatically excluded without evaluating individual items. The filter engine maintains an excluded-parents set to short-circuit evaluation during scanning.

### 6.3 Filter Changes and Stale Files

When filter rules change (patterns added/removed, sync_paths modified), files that are now excluded but still present locally become "stale." The sync engine detects this by comparing current filter config against the stored `config_snapshot` table ([data-model §8](data-model.md)).

```
function DetectStaleFiles(ctx, currentConfig, syncRoot):
    previousConfig = store.GetConfigSnapshot()

    if configFiltersMatch(currentConfig, previousConfig):
        return  // No filter changes

    // Filters changed — scan for newly stale files
    syncedItems = store.ListSyncedItems()

    for item in syncedItems:
        included, reason = filterEngine.ShouldSync(item.Path, item.ItemType == "folder", item.Size)
        if not included:
            // Item is now excluded but was previously synced
            if fileExists(join(syncRoot, item.Path)):
                store.RecordStale(StaleRecord{
                    ID:         newUUID(),
                    Path:       item.Path,
                    Reason:     reason,
                    DetectedAt: NowNano(),
                    Size:       item.LocalSize,
                })

    // Update config snapshot
    store.SaveConfigSnapshot(currentConfig)
```

**Key behavior**: Stale files are NEVER auto-deleted (safety). The user is nagged about them on every sync and via `status`. An explicit user interface allows disposition of each file (delete or keep). See [architecture §6.8](architecture.md).

### 6.4 Name Validation

OneDrive enforces naming restrictions that differ from POSIX filesystems. The scanner validates all names before processing:

| Rule | Details |
|------|---------|
| **Disallowed names** (case-insensitive) | `.lock`, `desktop.ini`, `CON`, `PRN`, `AUX`, `NUL`, `COM0`-`COM9`, `LPT0`-`LPT9` |
| **Disallowed patterns** | Names starting with `~$`; names containing `_vti_`; `forms` at root level |
| **Invalid characters** | `"`, `*`, `:`, `<`, `>`, `?`, `/`, `\`, `|` |
| **Invalid formatting** | Leading whitespace, trailing whitespace, trailing dot (`.`) |
| **Path length** | 400 characters for both Personal and Business (measured in characters, not bytes) |
| **Component length** | 255 bytes per path component (Linux filesystem limit) |

Items failing name validation are skipped with a warning. They are not synced and do not cause errors.

---

## 7. Conflict Detection and Resolution

### 7.1 Conflict Types

| Type | Detection Condition | Decision Matrix Row |
|------|-------------------|-------------------|
| **Edit-edit** | Both `localChanged` and `remoteChanged` are true, `local_hash != quick_xor_hash` | F5 |
| **Delete-edit** | Local deleted (no `local_hash`), remote changed | F7 (remote wins) |
| **Edit-delete** | Local changed, remote tombstoned | F9 |
| **Create-create** | Both sides created a file at the same path with no synced base, hashes differ | F11 |
| **Type change** | Path was a file, now a folder (or vice versa) | Decomposed: delete old type + create new type |
| **Case conflict** | Two local items would collide under case-insensitive rules | Detected during upload pre-check |

### 7.2 Resolution Policy

The default resolution policy is **keep-both**: both versions are preserved.

```
function ResolveConflict(item, conflictType) []Action:
    actions = []

    switch conflictType:

    case EditEdit:
        // Keep both: rename local, download remote
        conflictPath = generateConflictPath(item.Path)
        actions = append(actions,
            Action{Type: ActionLocalMove, Path: item.Path, NewPath: conflictPath},
            Action{Type: ActionDownload, Path: item.Path, Item: item},
        )

    case EditDelete:
        // Local was modified, remote was deleted
        // Keep local version, re-upload it
        actions = append(actions,
            Action{Type: ActionUpload, Path: item.Path, Item: item},
        )

    case CreateCreate:
        // Both created different files at same path
        // Rename local, download remote
        conflictPath = generateConflictPath(item.Path)
        actions = append(actions,
            Action{Type: ActionLocalMove, Path: item.Path, NewPath: conflictPath},
            Action{Type: ActionDownload, Path: item.Path, Item: item},
        )

    // Record in conflict ledger
    store.RecordConflict(ConflictRecord{
        ID:          newUUID(),
        DriveID:     item.DriveID,
        ItemID:      item.ItemID,
        Path:        item.Path,
        DetectedAt:  NowNano(),
        LocalHash:   item.LocalHash,
        RemoteHash:  item.QuickXorHash,
        LocalMtime:  item.LocalMtime,
        RemoteMtime: item.RemoteMtime,
        Resolution:  "keep_both",
        ResolvedBy:  "auto",
        History:     jsonAppend(nil, "detected", NowNano(), "auto"),
    })

    return actions
```

### 7.3 Conflict File Naming

Conflict copies use a deterministic, timestamp-based naming pattern:

```
<stem>.conflict-<YYYYMMDD-HHMMSS>.<ext>
```

Examples:
- `report.docx` → `report.conflict-20260217-143052.docx`
- `notes.txt` → `notes.conflict-20260217-143052.txt`
- `.bashrc` → `.bashrc.conflict-20260217-143052`

```
function generateConflictPath(originalPath) string:
    dir = dirname(originalPath)
    base = basename(originalPath)
    ext = extension(base)
    stem = base without ext
    timestamp = time.Now().UTC().Format("20060102-150405")

    candidate = join(dir, stem + ".conflict-" + timestamp + ext)

    // Collision avoidance
    counter = 1
    while fileExists(candidate) and counter < 1000:
        candidate = join(dir, stem + ".conflict-" + timestamp + "-" + itoa(counter) + ext)
        counter++

    return candidate
```

**Improvement over reference**: The reference uses hostname-based naming (`file-host-safeBackup-0001.ext`) which is verbose and host-specific. Our timestamp-based naming is shorter, self-documenting (you can see when the conflict occurred), and does not tie the file to a specific machine.

### 7.4 Resolution Commands

The `conflicts` and `resolve` commands provide user-facing conflict management:

```
$ onedrive-go conflicts
ID       Path                          Detected            Status
c1a2b3   /Documents/report.docx       2026-02-17 14:30    unresolved
c4d5e6   /Notes/meeting.md            2026-02-17 14:31    keep_both

$ onedrive-go resolve c1a2b3 --keep-local
Resolved: /Documents/report.docx → kept local version

$ onedrive-go resolve c1a2b3 --keep-remote
Resolved: /Documents/report.docx → kept remote version

$ onedrive-go resolve --all --keep-both
Resolved 2 conflicts: kept both versions
```

Resolution options:
- `--keep-local`: Upload local version, overwrite remote
- `--keep-remote`: Download remote version, overwrite local
- `--keep-both`: Keep both versions (default automatic behavior)
- `--all`: Apply resolution to all unresolved conflicts

### 7.5 False Conflict Handling

A "false conflict" occurs when both sides changed independently but arrived at the same content:

```
Both changed AND (local_hash == quick_xor_hash)
```

False conflicts are resolved silently by updating the synced base state. No transfer occurs. No conflict is recorded. This matches the pattern used by rclone bisync and Unison.

### 7.6 Case-Sensitivity Conflicts

OneDrive is case-insensitive; Linux is case-sensitive. Two local files `README.md` and `readme.md` would collide on OneDrive.

Detection: Before uploading a new file, the reconciler checks for case-insensitive collisions:

```
function checkCaseConflict(path, existingItems) error:
    dir = dirname(path)
    name = basename(path)
    siblings = store.ListChildren(dir)

    for sibling in siblings:
        if strings.EqualFold(sibling.Name, name) and sibling.Name != name:
            return CaseConflictError{
                Path:         path,
                ConflictWith: sibling.Path,
            }
    return nil
```

When a case conflict is detected, the upload is skipped with a warning. The user must rename one of the files to resolve the conflict.

---

## 8. Safety Checks (Pre-Execution Gates)

After the reconciler produces an action plan, the safety checks gate validates the plan before any actions are executed. If any gate fails, the sync cycle aborts.

### 8.1 Big-Delete Protection

Big-delete protection uses a **dual threshold**: if EITHER the absolute count OR the percentage of total items exceeds its threshold, the sync is aborted.

```
function CheckBigDelete(plan ActionPlan, config SafetyConfig, totalItems int) error:
    deleteCount = len(plan.LocalDeletes) + len(plan.RemoteDeletes)

    if deleteCount == 0:
        return nil

    // Minimum threshold: small drives should not trigger on tiny counts
    if totalItems < config.BigDeleteMinItems:
        return nil  // Drive too small for percentage-based protection

    // Either threshold triggers protection
    countExceeded = deleteCount > config.BigDeleteThreshold
    percentExceeded = totalItems > 0 and
                      (deleteCount * 100 / totalItems) > config.BigDeletePercentage

    if countExceeded or percentExceeded:
        return BigDeleteError{
            Count:      deleteCount,
            Total:      totalItems,
            Threshold:  config.BigDeleteThreshold,
            Percentage: config.BigDeletePercentage,
        }

    return nil
```

Default configuration:
- `big_delete_threshold`: 1000 items
- `big_delete_percentage`: 50%
- `big_delete_min_items`: 10 (drives with fewer items skip this check)

When triggered, the sync aborts with a message:

```
ERROR: This sync would delete 2,847 files (57% of 4,995 total).
This exceeds the big-delete threshold (1000 items OR 50%).
Run with --force to proceed, or adjust safety thresholds in config.
```

### 8.2 Disk Space Check

Disk space is checked per-file at execution time (not at planning time, because space may be freed by deletions):

```
function CheckDiskSpace(path string, requiredBytes int64) error:
    available = getAvailableDiskSpace(dirname(path))
    minFree = config.MinFreeSpace  // Default 1GB

    if available - requiredBytes < minFree:
        return DiskSpaceError{
            Path:      path,
            Required:  requiredBytes,
            Available: available,
            MinFree:   minFree,
        }
    return nil
```

### 8.3 Incomplete Delta Guard

Safety invariant **S2**: Never process deletions from an incomplete delta response.

```
function CheckDeltaComplete(plan ActionPlan, driveID string) error:
    deltaComplete = store.GetDeltaComplete(driveID)

    if not deltaComplete:
        // Remove all local delete actions (items deleted remotely)
        plan.LocalDeletes = filterOutRemoteDeletions(plan.LocalDeletes)
        log.Warn("delta incomplete, suppressing remote deletion processing",
                 "driveID", driveID,
                 "suppressed", suppressedCount)
    return nil
```

### 8.4 Hash-Before-Delete Guard

Safety invariant **S4**: Before deleting a local file due to remote deletion, verify the local content matches the synced state.

```
function HashBeforeDelete(item Item, syncRoot string) (bool, string):
    fullPath = join(syncRoot, item.Path)

    if not fileExists(fullPath):
        return true, ""  // Already gone, safe to proceed with DB cleanup

    localHash = computeQuickXorHash(fullPath)

    if localHash == item.SyncedHash:
        return true, ""  // Content matches synced state, safe to delete

    // Content differs — user modified since last sync
    // Back up instead of deleting
    return false, localHash
```

When the guard detects a modified file:
1. The file is renamed to a conflict path (same naming as [§7.3](#73-conflict-file-naming))
2. The renamed file is treated as a new local file on the next sync
3. The conflict is recorded in the conflict ledger (type: edit-delete)
4. The original file path is freed for any remote operations

---

## 9. Execution

The executor processes the action plan produced by the reconciler, after it passes safety checks.

### 9.1 Execution Order

Actions are executed in a strict order to maintain correctness:

```
function Execute(ctx, plan ActionPlan, syncRoot string) SyncReport:
    report = SyncReport{}

    // Phase 1: Folder creates (top-down, sequential)
    for action in plan.FolderCreates:
        executeFolderCreate(ctx, action, syncRoot)
        report.FoldersCreated++

    // Phase 2: Moves/renames (sequential)
    for action in plan.Moves:
        executeMove(ctx, action, syncRoot)
        report.Moved++

    // Phase 3: Downloads and uploads (parallel via worker pools)
    downloadResults = transferManager.DownloadAll(ctx, plan.Downloads)
    uploadResults = transferManager.UploadAll(ctx, plan.Uploads)

    for result in downloadResults:
        if result.OK:
            report.Downloaded++
            report.BytesDownloaded += result.Size
        else:
            report.Errors++

    for result in uploadResults:
        if result.OK:
            report.Uploaded++
            report.BytesUploaded += result.Size
        else:
            report.Errors++

    // Phase 4: Local deletes (files first, then folders bottom-up)
    for action in plan.LocalDeletes:
        executeLocalDelete(ctx, action, syncRoot)
        report.LocalDeleted++

    // Phase 5: Remote deletes (files first, then folders bottom-up)
    for action in plan.RemoteDeletes:
        executeRemoteDelete(ctx, action)
        report.RemoteDeleted++

    // Phase 6: Conflict resolution
    for action in plan.Conflicts:
        executeConflict(ctx, action, syncRoot)
        report.Conflicts++

    // Phase 7: Synced state updates (false conflicts, bookkeeping)
    for action in plan.SyncedUpdates:
        updateSyncedState(action.Item)

    // Phase 8: DB cleanup
    for action in plan.Cleanups:
        store.DeleteItem(action.DriveID, action.ItemID)

    return report
```

**Why this order?**
1. **Folders first**: Downloads and uploads need parent directories to exist.
2. **Moves before transfers**: Avoids downloading to a path that is about to be renamed.
3. **Transfers in parallel**: Downloads and uploads are independent and benefit from parallelism.
4. **Deletes after transfers**: Frees paths that may have been occupied, and ensures all transfers complete before cleanup.

### 9.2 Download Execution

Downloads follow the atomic write pattern to satisfy safety invariant **S3**:

```
function executeDownload(ctx, action Action, syncRoot string) DownloadResult:
    item = action.Item
    finalPath = join(syncRoot, item.Path)
    partialPath = finalPath + ".partial"

    // S6: Disk space check
    err = CheckDiskSpace(finalPath, item.Size)
    if err != nil:
        return DownloadResult{OK: false, Error: err}

    // Download to .partial file
    file = createFile(partialPath)
    hasher = quickxorhash.New()
    writer = io.MultiWriter(file, hasher)

    err = api.DownloadFile(ctx, item.DriveID, item.ItemID, writer)
    file.Close()

    if err != nil:
        removeFile(partialPath)
        return DownloadResult{OK: false, Error: err}

    // Verify hash
    computedHash = base64.Encode(hasher.Sum(nil))

    if item.QuickXorHash != "" and computedHash != item.QuickXorHash:
        // Hash mismatch — handle known API bugs
        if isKnownHashMismatchType(item):
            // iOS .heic files, SharePoint enriched files
            log.Warn("hash mismatch for known-buggy file type",
                     "path", item.Path, "expected", item.QuickXorHash,
                     "got", computedHash)
        else:
            removeFile(partialPath)
            return DownloadResult{OK: false, Error: HashMismatchError{...}}

    // Atomic rename
    err = os.Rename(partialPath, finalPath)
    if err != nil:
        removeFile(partialPath)
        return DownloadResult{OK: false, Error: err}

    // Restore timestamps
    mtime = time.Unix(0, item.RemoteMtime)
    os.Chtimes(finalPath, mtime, mtime)

    // Update state DB
    item.LocalHash = computedHash
    item.LocalSize = item.Size
    item.LocalMtime = item.RemoteMtime
    item.SyncedHash = computedHash
    item.SyncedSize = item.Size
    item.SyncedMtime = item.RemoteMtime
    item.LastSyncedAt = NowNano()
    store.UpsertItem(item)

    return DownloadResult{OK: true, Size: item.Size}
```

### 9.3 Upload Execution

Uploads use simple PUT for files ≤ 4MB, and resumable upload sessions for larger files:

```
function executeUpload(ctx, action Action, syncRoot string) UploadResult:
    item = action.Item
    localPath = join(syncRoot, item.Path)

    // Read local file and compute hash
    file = openFile(localPath)
    stat = file.Stat()
    hasher = quickxorhash.New()
    reader = io.TeeReader(file, hasher)

    if stat.Size <= 4 * 1024 * 1024:
        // Simple upload
        response = api.SimpleUpload(ctx, item.DriveID, item.ParentID, item.Name, reader)
    else:
        // Resumable upload session
        session = api.CreateUploadSession(ctx, item.DriveID, item.ParentID, item.Name)
        store.SaveUploadSession(session)  // Persist for crash recovery
        response = api.UploadSessionFragments(ctx, session, reader, stat.Size)
        store.DeleteUploadSession(session.ID)

    file.Close()
    localHash = base64.Encode(hasher.Sum(nil))

    if response.Error != nil:
        return UploadResult{OK: false, Error: response.Error}

    // Post-upload verification
    serverHash = response.Item.File.Hashes.QuickXorHash

    // Log enrichment if detected (informational only — no corrective action)
    if serverHash != "" and serverHash != localHash:
        if isSharePointLibrary(item.DriveID):
            log.Info("SharePoint enrichment detected; local file preserved",
                     "path", item.Path,
                     "localHash", localHash,
                     "serverHash", serverHash,
                     "localSize", stat.Size,
                     "serverSize", response.Item.Size)
        else:
            log.Warn("upload hash mismatch on non-SharePoint drive",
                     "path", item.Path,
                     "local", localHash,
                     "server", serverHash)

    // Update state DB — store per-side truth
    item.ItemID       = response.Item.ID               // May be assigned for new files
    item.QuickXorHash = serverHash                      // Remote truth (may be enriched)
    item.Size         = response.Item.Size              // Remote size (may be enriched)
    item.ETag         = response.Item.ETag
    item.RemoteMtime  = parseTimestamp(response.Item.FileSystemInfo.LastModifiedDateTime)
    item.LocalHash    = localHash                       // Local truth (what's on disk)
    item.LocalSize    = stat.Size                       // Local size (what's on disk)
    item.LocalMtime   = toUnixNano(stat.ModTime)
    item.SyncedHash   = serverHash                      // Record server state as reference
    item.SyncedSize   = response.Item.Size
    item.SyncedMtime  = item.RemoteMtime
    item.LastSyncedAt = NowNano()
    store.UpsertItem(item)

    return UploadResult{OK: true, Size: stat.Size}
```

**Upload fragment alignment**: Fragments for resumable uploads must be multiples of 320 KiB (327,680 bytes) per API requirement. The transfer manager enforces this in chunk size validation. Default chunk size is 10 MiB.

**SharePoint enrichment handling**: After uploading to SharePoint, the server may modify the file by injecting library metadata, changing its content and hash. When post-upload verification detects a hash mismatch on a SharePoint library, we **log the enrichment and store per-side hashes** — `LocalHash` records what is actually on disk, `QuickXorHash` records the server's (enriched) hash. No download of the enriched version occurs. The per-side merge baselines ([section 5.1](#51-three-way-merge-algorithm)) ensure that the next sync cycle sees no change on either side, preventing infinite loops without modifying the user's local file. See [SHAREPOINT_ENRICHMENT.md](../../SHAREPOINT_ENRICHMENT.md) for the full design rationale.

### 9.4 Local Deletion Execution

Local deletions satisfy safety invariant **S4** (hash-before-delete):

```
function executeLocalDelete(ctx, action Action, syncRoot string) error:
    item = action.Item
    fullPath = join(syncRoot, item.Path)

    if not fileExists(fullPath):
        // Already gone — just clean up DB
        store.MarkDeleted(item.DriveID, item.ItemID, NowNano())
        return nil

    // S4: Hash-before-delete guard
    safe, localHash = HashBeforeDelete(item, syncRoot)

    if not safe:
        // Local file was modified since last sync
        // Back up instead of deleting
        conflictPath = generateConflictPath(fullPath)
        os.Rename(fullPath, conflictPath)

        store.RecordConflict(ConflictRecord{
            ID:         newUUID(),
            DriveID:    item.DriveID,
            ItemID:     item.ItemID,
            Path:       item.Path,
            DetectedAt: NowNano(),
            LocalHash:  localHash,
            RemoteHash: "",  // Remote deleted
            Resolution: "keep_both",
            ResolvedBy: "auto",
        })

        store.MarkDeleted(item.DriveID, item.ItemID, NowNano())
        return nil

    // Safe to delete
    if config.UseLocalTrash:
        moveToTrash(fullPath)  // OS trash (FreeDesktop / macOS Finder)
    else:
        os.Remove(fullPath)

    store.MarkDeleted(item.DriveID, item.ItemID, NowNano())
    return nil
```

### 9.5 Remote Deletion Execution

```
function executeRemoteDelete(ctx, action Action) error:
    item = action.Item

    if config.UseRecycleBin:
        err = api.DeleteToRecycleBin(ctx, item.DriveID, item.ItemID, item.ETag)
    else:
        err = api.PermanentDelete(ctx, item.DriveID, item.ItemID, item.ETag)

    if err != nil:
        switch classifyHTTPError(err):
        case 404:
            // Already deleted remotely — not an error
            log.Debug("item already deleted remotely", "path", item.Path)
        case 403:
            // Permission denied (retention policy, read-only share)
            log.Warn("cannot delete remote item (permission denied)",
                     "path", item.Path)
            return nil  // Skip, do not retry
        case 423:
            // Locked (SharePoint)
            log.Warn("cannot delete remote item (locked)",
                     "path", item.Path)
            return nil  // Skip, do not retry
        case 412:
            // eTag stale — refresh and retry
            freshItem = api.GetItem(ctx, item.DriveID, item.ItemID)
            if freshItem != nil:
                return executeRemoteDeleteWithETag(ctx, item, freshItem.ETag)
            return nil
        default:
            return err  // Retryable or fatal per error tier
        }

    store.MarkDeleted(item.DriveID, item.ItemID, NowNano())
    return nil
```

### 9.6 Error Handling

Errors during execution are classified into four tiers per [architecture §7](architecture.md):

| Tier | Examples | Response | Retry? |
|------|----------|----------|--------|
| **Fatal** | Auth failure (401 after refresh), DB corruption, impossible state | Stop entire sync, alert user, exit non-zero | No |
| **Retryable** | Network timeout, HTTP 429/500/502/503/504, HTTP 408 | Exponential backoff + jitter, respect Retry-After | Yes (max 5) |
| **Skip** | Permission denied (403), invalid filename (400), locked (423), path too long | Log warning, skip item, continue sync | No |
| **Deferred** | Parent dir not yet created, file locked locally | Queue for retry at end of current sync cycle | Once |

**Retry strategy**:
- Base: 1 second
- Factor: 2 (exponential)
- Max backoff: 120 seconds
- Jitter: ±25% of calculated backoff
- Max retries: 5 per operation
- For HTTP 429: Use `Retry-After` header directly instead of calculated backoff
- Global rate awareness: A shared token bucket across all workers prevents thundering herd

```
function retryWithBackoff(operation func() error, maxRetries int) error:
    for attempt = 0; attempt < maxRetries; attempt++:
        err = operation()
        if err == nil:
            return nil

        if not isRetryable(err):
            return err

        delay = calculateBackoff(attempt)
        if retryAfter = getRetryAfter(err); retryAfter > 0:
            delay = retryAfter

        sleep(delay + jitter(delay * 0.25))

    return MaxRetriesExceeded{Attempts: maxRetries, LastError: err}

function calculateBackoff(attempt int) time.Duration:
    base = 1 * time.Second
    backoff = base * (1 << attempt)  // 1s, 2s, 4s, 8s, 16s, 32s, 64s, 120s
    if backoff > 120 * time.Second:
        backoff = 120 * time.Second
    return backoff
```

---

## 10. Initial Sync (First Run)

On first run with a new profile, there is no delta token and no state database entries. The initial sync establishes the baseline for all future delta-based syncs.

### 10.1 First Run Detection

```
function IsInitialSync(driveID string) bool:
    token = store.GetDeltaToken(driveID)
    return token == ""
```

### 10.2 Full Enumeration via Delta

When no delta token exists, the Graph API delta endpoint returns every item in the drive. This is functionally a full enumeration:

```
function InitialSync(ctx, driveID, syncRoot):
    // Phase 1: Fetch all remote items via delta (no token → full enumeration)
    FetchAndApplyDeltas(ctx, driveID)

    // Phase 2: Scan local filesystem
    ScanLocalFilesystem(ctx, syncRoot, filterEngine)
    DetectLocalDeletions(ctx, syncRoot)

    // Phase 3: Reconcile
    // No synced base exists for any item, so:
    // - Remote-only items → Download (row F13)
    // - Local-only items → Upload (row F12)
    // - Both exist with same hash → Record as synced (row F10)
    // - Both exist with different hash → Create-create conflict (row F11)
    plan = Reconcile(ctx, mode)

    // Phase 4: Safety checks and execute
    CheckBigDelete(plan, config, totalItems)
    Execute(ctx, plan, syncRoot)
```

### 10.3 Parallel Children Enumeration (Optimization)

For maximum initial sync speed, an alternative to the delta-based full enumeration uses parallel `/children` API calls with multiple walker goroutines:

```
function ParallelEnumerate(ctx, driveID, rootItemID):
    queue = channel[string]{rootItemID}  // Start with root
    walkers = 8  // Configurable

    for i = 0; i < walkers; i++:
        go func():
            for parentID in queue:
                children = api.ListChildren(ctx, driveID, parentID)
                for child in children:
                    normalized = normalize.NormalizeItem(child)
                    store.UpsertItem(createItemFromNormalized(normalized))

                    if child.Folder != nil:
                        queue <- child.ID  // Enqueue folder for walking

            close(queue) when all walkers idle
```

This is used when the delta-based enumeration would be slower (very large drives). The engine falls back to delta-based enumeration if `/children` enumeration fails or if the number of items exceeds 300,000 (Microsoft's recommended delta limit).

### 10.4 Saving Initial Delta Token

After the initial sync completes, a delta token must be obtained for future syncs. Rather than re-fetching everything, the engine requests a token representing "now":

```
function SaveInitialDeltaToken(ctx, driveID):
    // Request delta with ?token=latest
    // Returns empty items + a deltaLink with a fresh token
    _, deltaLink = api.DeltaPage(ctx, driveID, "latest")
    token = extractToken(deltaLink)
    store.SaveDeltaToken(driveID, token)
    store.SetDeltaComplete(driveID, true)
```

This token captures the state at the end of the initial sync. The next delta call will return only changes that occurred after this point.

---

## 11. Continuous Mode (`--watch`)

Continuous mode keeps the sync engine running, reacting to both local and remote changes in real time.

### 11.1 Event Loop

```
function RunWatch(ctx, mode SyncMode, syncRoot string):
    // Perform initial one-shot sync
    RunOnce(ctx, mode)

    // Start watchers
    localEvents = monitor.WatchLocal(ctx, syncRoot)     // inotify/FSEvents
    remoteEvents = monitor.WatchRemote(ctx, driveID)    // WebSocket + poll

    // Main event loop
    for:
        select:
        case <-ctx.Done():
            return GracefulShutdown(ctx)

        case batch := <-localEvents:
            // Local filesystem change detected
            // Debounced: events are batched with 2-second window
            handleLocalChanges(ctx, mode, syncRoot, batch)

        case <-remoteEvents:
            // Remote change notification received
            // Fetch delta from last token
            handleRemoteChanges(ctx, mode, syncRoot)

        case <-configReload:
            // SIGHUP received
            handleConfigReload(ctx, syncRoot)
```

**Event sources**:

| Source | Mechanism | Latency | Reliability |
|--------|----------|---------|-------------|
| **Local filesystem** | inotify (Linux) / FSEvents (macOS) / kqueue (FreeBSD) via `rjeczalik/notify` | ~2 seconds (debounce window) | High on local fs, limited on NFS |
| **Remote (WebSocket)** | Microsoft Graph WebSocket subscription | Near-instant | Best-effort, may disconnect |
| **Remote (poll)** | Delta API polling on timer | Configurable, default 5 minutes | Reliable fallback |

### 11.2 Local Change Handling

```
function handleLocalChanges(ctx, mode, syncRoot, events []FSEvent):
    // Coalesce events: if a file was created and deleted within
    // the debounce window, ignore both
    coalesced = coalesceEvents(events)

    // For each changed path, re-scan and reconcile
    for event in coalesced:
        relPath = relativize(event.Path, syncRoot)

        // Re-scan the specific path
        processLocalFile(ctx, event.Path, relPath, stat(event.Path))

    // Reconcile only affected items
    plan = ReconcileSubset(ctx, mode, affectedPaths)
    if plan.HasActions():
        Execute(ctx, plan, syncRoot)
```

**Debounce window** (2 seconds, per [architecture §3.6](architecture.md)):
- Events within the window are batched together
- Files created and deleted within the window are ignored (handles temp files, atomic saves)
- Events for the same path are deduplicated (only the final state matters)

### 11.3 Idle CPU Prevention

100% CPU when idle is a commonly reported bug in sync tools. Our architecture prevents this by design:

1. **Blocking I/O only**: The event loop uses Go's `select` statement on channels. When no events are pending, the goroutine blocks and consumes zero CPU.
2. **No polling loops**: Local changes come from OS-level filesystem events (inotify/FSEvents), not periodic directory walks. Remote changes come from WebSocket push or timer-based poll (not busy-wait).
3. **Debounce via timer**: The 2-second debounce uses `time.AfterFunc`, not a tight loop checking timestamps.
4. **Hard NFR**: <1% idle CPU. If profiling reveals CPU usage above this threshold, it is treated as a bug.

### 11.4 Graceful Shutdown

```
function GracefulShutdown(ctx):
    // First signal (SIGINT/SIGTERM): graceful drain
    shutdownCtx, cancel = context.WithTimeout(ctx, config.ShutdownTimeout)

    // Stop accepting new work
    monitor.StopWatching()

    // Wait for in-flight transfers to complete
    transferManager.Drain(shutdownCtx)

    // Save checkpoint
    store.SaveDeltaToken(driveID, currentToken)

    // Clean up .partial files
    cleanPartialFiles(syncRoot)

    cancel()
    return nil

    // Second signal: immediate exit
    // SQLite WAL ensures DB consistency even on abrupt termination
```

Signal handling follows [architecture §9.4](architecture.md):

| Signal | Action |
|--------|--------|
| First SIGINT/SIGTERM | Graceful: drain transfers (configurable timeout), save checkpoint, exit |
| Second SIGINT/SIGTERM | Immediate: cancel all operations, exit. WAL ensures DB consistency. |
| SIGHUP | Reload configuration, re-initialize filter engine, detect stale files |

### 11.5 Config Reload (SIGHUP)

```
function handleConfigReload(ctx, syncRoot):
    log.Info("reloading configuration (SIGHUP)")

    // Reload config file
    newConfig = config.Reload()

    // Re-initialize filter engine
    filterEngine = filter.NewEngine(newConfig.FilterConfig)

    // Detect stale files from filter changes
    DetectStaleFiles(ctx, newConfig, syncRoot)

    // Re-initialize bandwidth scheduler
    transferManager.UpdateBandwidth(newConfig.TransferConfig)

    log.Info("configuration reloaded successfully")
```

---

## 12. Crash Recovery

The sync engine is designed for crash recovery at any point in the pipeline. SQLite WAL mode ensures database consistency even on abrupt termination.

### 12.1 Recovery Procedure

```
function RecoverFromCrash(ctx, syncRoot):
    // Step 1: SQLite WAL recovery (automatic)
    // modernc.org/sqlite handles WAL replay on database open

    // Step 2: Clean up .partial files
    partialFiles = glob(syncRoot, "**/*.partial")
    for file in partialFiles:
        os.Remove(file)  // Incomplete downloads, safe to delete

    // Step 3: Resume or expire upload sessions
    sessions = store.ListUploadSessions()
    for session in sessions:
        if session.Expiry < NowNano():
            // Session expired, clean up
            store.DeleteUploadSession(session.ID)
        else:
            // Session still valid, will be resumed during execution
            log.Info("resuming upload session", "path", session.LocalPath,
                     "progress", session.BytesUploaded, "/", session.TotalSize)

    // Step 4: Reload delta token
    // Re-fetch from last saved token — at most one batch may need reprocessing
    // This is idempotent because of upsert semantics
```

### 12.2 Crash Recovery Guarantees

| Crash Point | Recovery Behavior | Data Loss? |
|-------------|-------------------|:----------:|
| During delta fetch (mid-page) | Re-fetch from last saved token. Partial page not committed. | No |
| During delta apply (mid-batch) | Re-fetch from last saved token. At most 500 items reprocessed (idempotent). | No |
| During local scan | Re-scan entire filesystem. Local state is re-detected. | No |
| During reconciliation | Re-reconcile from current DB state. Plan is re-generated. | No |
| During download | `.partial` file is cleaned up. File re-downloaded on next cycle. | No |
| During upload (simple) | Upload is retried on next cycle. Server may have a partial or no item. | No |
| During upload (session) | Session is resumed from `bytes_uploaded` if not expired. Otherwise restarted. | No |
| During local delete | File may or may not be deleted. DB marks it as deleted regardless. Worst case: file exists locally but DB says deleted → re-downloaded on next cycle. | No |
| During remote delete | Item may or may not be deleted. DB marks it as deleted. Next delta will confirm. | No |
| After delta token saved | Clean state. Next cycle picks up from saved token. | No |

### 12.3 WAL Guarantees

SQLite with WAL mode and `PRAGMA synchronous = FULL` provides:

- **Atomicity**: Each transaction is all-or-nothing. A crash during a transaction rolls back all changes in that transaction.
- **Durability**: Committed transactions survive power loss. `FULL` synchronous ensures the WAL is fsynced before acknowledging a commit.
- **Consistency**: The database is always in a valid state after WAL replay.

The batch processing model ([§2.3](#23-batch-processing-model)) ensures that at most one batch of items (default 500) needs reprocessing after a crash. Each batch is committed in a single transaction, so either all items in the batch are applied or none are.

---

## 13. Sync Report

Every sync cycle produces a `SyncReport` with counters for all operations performed.

### 13.1 SyncReport Structure

```go
type SyncReport struct {
    // Timing
    StartedAt   int64         // Unix nanoseconds
    CompletedAt int64         // Unix nanoseconds
    Duration    time.Duration

    // Transfer counters
    Downloaded      int
    Uploaded        int
    BytesDownloaded int64
    BytesUploaded   int64

    // Operation counters
    LocalDeleted    int
    RemoteDeleted   int
    Moved           int
    FoldersCreated  int

    // Conflict counters
    Conflicts       int
    FalseConflicts  int

    // Error counters
    Errors          int
    Skipped         int
    Retried         int

    // State
    TotalItems      int
    StaleFiles      int
    UnresolvedConflicts int

    // Mode
    Mode            SyncMode
    DryRun          bool
    Profile         string
}
```

### 13.2 Output Formats

**Interactive mode** (human-readable to stderr):

```
Sync complete (profile "default", bidirectional)
  ↓ 3 downloaded (12.4 MB)    ↑ 2 uploaded (8.1 MB)
  × 1 conflict                 ⊘ 1 deleted locally
  Duration: 4.2s
  Unresolved conflicts: 1 (run `onedrive-go conflicts`)
```

**JSON mode** (`--json` or `--quiet`):

```json
{
  "profile": "default",
  "mode": "bidirectional",
  "dry_run": false,
  "duration_ms": 4200,
  "downloaded": 3,
  "uploaded": 2,
  "bytes_downloaded": 13003776,
  "bytes_uploaded": 8493056,
  "local_deleted": 1,
  "remote_deleted": 0,
  "moved": 0,
  "conflicts": 1,
  "errors": 0,
  "total_items": 4995,
  "unresolved_conflicts": 1
}
```

---

## 14. Verify Command

The `verify` command performs a full-tree integrity check, comparing local files against the state database and remote hashes. It is a read-only operation that does not modify any files or state.

### 14.1 Verify Algorithm

```
function Verify(ctx, syncRoot string) VerifyReport:
    report = VerifyReport{}
    items = store.ListAllFiles()  // All non-deleted file items

    // Use checker pool for parallel hashing
    for item in items:
        fullPath = join(syncRoot, item.Path)

        // Check 1: File exists locally
        if not fileExists(fullPath):
            report.Missing = append(report.Missing, item.Path)
            continue

        // Check 2: Size matches
        stat = os.Stat(fullPath)
        if stat.Size != item.Size:
            report.SizeMismatch = append(report.SizeMismatch, VerifyMismatch{
                Path:     item.Path,
                Expected: item.Size,
                Actual:   stat.Size,
            })

        // Check 3: Local hash matches DB hash
        localHash = computeQuickXorHash(fullPath)  // Via checker pool

        if localHash != item.LocalHash:
            report.LocalHashMismatch = append(report.LocalHashMismatch, VerifyMismatch{
                Path:     item.Path,
                Expected: item.LocalHash,
                Actual:   localHash,
            })

        // Check 4: Local hash matches remote hash
        if item.QuickXorHash != "" and localHash != item.QuickXorHash:
            report.RemoteHashMismatch = append(report.RemoteHashMismatch, VerifyMismatch{
                Path:     item.Path,
                Expected: item.QuickXorHash,
                Actual:   localHash,
            })

        // Check 5: Local hash matches synced hash
        if item.SyncedHash != "" and localHash != item.SyncedHash:
            report.SyncedHashMismatch = append(report.SyncedHashMismatch, VerifyMismatch{
                Path:     item.Path,
                Expected: item.SyncedHash,
                Actual:   localHash,
            })

        report.Verified++

    return report
```

### 14.2 Verify Report

```
$ onedrive-go verify
Verifying profile "default"...
  Verified: 4,993 files OK
  Missing locally: 1 file
    /Documents/deleted-remotely.pdf
  Hash mismatch (local vs remote): 1 file
    /Photos/image.heic (known iOS API bug)
  Total: 4,995 files checked in 12.4s
```

### 14.3 Periodic Verification

Opt-in configurable full-tree hash verification on a schedule:

```toml
[sync]
verify_interval = "7d"  # Run verify weekly (default: disabled)
```

When enabled, the verify command runs automatically at the configured interval during `--watch` mode. Results are logged and included in the sync report.

---

## Appendix A: Architecture Constraint Traceability

Every constraint from [architecture.md §19](architecture.md) "For `sync-algorithm.md`" is traced to its implementation in this document.

| Architecture Constraint | Reference | Implementation |
|------------------------|-----------|----------------|
| Three-way merge (local vs remote vs last-known) | §19 | §5 Reconciliation — full three-way merge with decision matrix |
| Process delta items in received order, with deletion reordering at page boundaries | §19 | §3.3 Deletion Reordering — buffer page, partition, process deletions first |
| Configurable batch size (default 500) with checkpoints | §19, §6.5 | §2.3 Batch Processing Model, §3.1 Delta Fetch Loop |
| Handle both resync types on HTTP 410 | §19, §7.4 | §3.5 HTTP 410 Handling |
| Four-tier error model | §19, §7 | §9.6 Error Handling |
| Never delete remote based on download state | §19, §7.3 | §4.3 Orphan Detection (S1), §8.4 Hash-Before-Delete Guard (S4) |
| Parallel worker pools (uploads, downloads, checkers) | §5.2 | §9.1 Execution Order (parallel Phase 3) |
| Single writer goroutine per profile | §5.1 | All DB writes go through store interface (serialized) |
| Context tree with cancellation propagation | §5.4 | §2.4 Context Tree |
| Bounded channels for backpressure | §5.3 | §2.4 Pipeline stages connected via bounded channels |
| Graceful shutdown (two-signal protocol) | §5.5 | §11.4 Graceful Shutdown |
| SIGHUP config reload | §5.5, §9.4 | §11.5 Config Reload |
| Delta token saved per drive | §6.5 | §3.1 SaveDeltaToken per driveID |
| Materialized paths with cascading update | §6.4 | §3.4 ApplyDeltaItem — path recomputation and cascade |
| Tombstones for move detection | §6.6 | §3.4 Resurrection from tombstone, §5.4 Tombstone-Based Move Detection |
| Conflict ledger | §6.7 | §7 Conflict Detection and Resolution |
| Stale files ledger | §6.8 | §6.3 Filter Changes and Stale Files |
| Normalization layer between API and sync engine | §8 | §3.2 Normalization table |
| All known API quirks handled | §8 | §3.2 full quirk normalization table |
| Atomic file writes (.partial + hash + rename) | §7.3 | §9.2 Download Execution (S3) |
| Big-delete protection (count AND percentage) | §13.1 | §8.1 Big-Delete Protection |
| Disk space check before download | §13.2 | §8.2 Disk Space Check (S6) |
| OS trash for local deletions | §13.3 | §9.4 Local Deletion Execution |
| Verify command | §13.4 | §14 Verify Command |
| Crash recovery via WAL | §13.5 | §12 Crash Recovery |
| Layered filter evaluation | §11 | §6 Filtering |
| Filter change → stale files ledger | §11.2 | §6.3 Filter Changes and Stale Files |
| Move detection by item ID | §12 | §5.4 Move/Rename Detection |
| WebSocket + 5-min poll for remote changes | §3.6 | §11.1 Event Loop |
| 2-second debounce for local events | §3.6 | §11.2 Local Change Handling |

---

## Appendix B: Design Differences

Key design differences between our algorithm and alternative approaches, with rationale.

| Aspect | Alternative Approach | onedrive-go | Rationale |
|--------|---------------------|-------------|-----------|
| **Pipeline model** | 6-pass sequential (delta → download → shared → consistency → scan → true-up) | 3-stage pipeline (fetch+scan → reconcile → execute) | Simpler, eliminates redundant passes. The "true-up" pass is unnecessary because we save the delta token only after complete processing. |
| **Merge algorithm** | Two-way: compare local vs remote, use timestamps for tiebreaking | Three-way: compare local vs remote vs synced base | Three-way merge enables precise conflict detection. Two-way cannot distinguish "both changed" from "one changed" without a common ancestor. |
| **Move detection (remote)** | eTag + path comparison | Tombstone-based: same item ID appears at new location | More reliable. eTag changes for many reasons besides moves. Tombstones provide a clear signal. |
| **Move detection (local)** | inotify events (monitor mode only); no detection in one-shot | Hash-matching across deleted and new items | Works in both one-shot and watch modes. Does not depend on filesystem events. |
| **Path reconstruction** | O(depth) parent-chain walk per access, no caching | Materialized paths stored in DB, cascading update on change | O(1) path lookup. Eliminates a major performance bottleneck at scale. |
| **Filter changes** | Require destructive `--resync` | Stale files ledger: detect, nag, explicit disposition | No data loss. No full re-enumeration. User controls cleanup. |
| **Conflict naming** | `file-hostname-safeBackup-0001.ext` | `file.conflict-20260217-143052.ext` | Shorter, self-documenting timestamp, not tied to hostname. |
| **Conflict tracking** | Creates backup files, no ledger | Full conflict ledger with resolution tracking | Users can list, review, and explicitly resolve conflicts. |
| **Download safety** | Writes directly to target path | `.partial` + hash verify + atomic rename | Prevents corruption from interrupted downloads. |
| **Delta token handling** | Saves after each page | Saves only after complete response (all pages through deltaLink) | Prevents partial state from being treated as complete. |
| **HTTP 410 recovery** | Always full re-enumeration, ignores resync type | Differentiates resync types, handles each appropriately | More efficient recovery for large drives. |
| **Deletion safety** | Can delete remote files based on failed downloads | Never deletes remote based on local absence without synced-base confirmation | Prevents the most catastrophic data loss scenario. |
| **Database writes** | Per-operation mutex | Single writer goroutine, concurrent readers via WAL | Eliminates contention, simplifies concurrency. |
| **Rate limiting** | Per-thread independent retry | Global shared token bucket | Prevents thundering herd on rate limit responses. |
| **Big-delete** | Count-only threshold | Dual threshold: count OR percentage, with minimum items | More conservative: either exceeding 1000 items or 50% of the drive triggers protection. Catches both large absolute counts and proportionally large deletions. |
| **Initial sync** | Sequential delta + sequential download | Parallel `/children` enumeration + parallel download | Significantly faster for large drives. |
| **SharePoint enrichment** | Disable validation flag | Per-side baselines; no enrichment-specific code path. See [SHAREPOINT_ENRICHMENT.md](../../SHAREPOINT_ENRICHMENT.md). | Per-side merge baselines (`LocalHash` for local, `QuickXorHash` for remote) handle enrichment naturally. No extra download, no local file modification, no enrichment-specific detection code. Prevents infinite loop by construction. |

---

## Appendix C: Decision Log

Numbered rationale for key algorithm design choices.

| # | Decision | Rationale |
|---|----------|-----------|
| D1 | Three-way merge instead of two-way | Two-way comparison cannot distinguish "A changed, B didn't" from "both changed to same thing." The synced base provides the common ancestor needed for precise change detection. This is the universal best practice in sync tools. |
| D2 | Pipeline (fetch→reconcile→execute) instead of interleaved | Separating concerns makes the algorithm easier to reason about, test, and debug. The reconciler sees a consistent snapshot of both sides. The executor can parallelize all transfers. |
| D3 | Delta token saved only after complete response | Saving after each page creates a window where the delta token advances past items that were fetched but not processed. If the process crashes, those items are lost. Saving only on `deltaLink` ensures completeness. |
| D4 | Deletion reordering at page level only | Cross-page reordering would break the parent-before-child guarantee that the API provides for creations. Page-level reordering handles the known bug (deletions after creations at same path) without violating ordering invariants. |
| D5 | Tombstones for 30 days instead of immediate hard delete | Tombstones enable move detection across sync cycles. Without them, a delete-then-reappear sequence would be treated as delete + create (causing unnecessary re-download). 30 days is long enough for any reasonable move detection while not growing the DB indefinitely. |
| D6 | Hash-based local move detection with unique-match constraint | Without filesystem-level item IDs, hash matching is the only way to detect local moves. Requiring unique hash matches prevents ambiguous matches from being treated as moves (which could cause data loss if wrong). |
| D7 | Materialized paths instead of parent-chain walks | At 100K items with average depth 5, parent-chain walks would require 500K DB queries per sync cycle just for path resolution. Materialized paths with cascading updates reduce this to O(1) per item. |
| D8 | Dual big-delete threshold (count OR percentage) | Count-only misses proportionally large deletions on small drives. Percentage-only fires on tiny absolute counts. Using OR means either a large absolute count (>1000) or a large proportion (>50%) triggers protection. A minimum-items guard (10) prevents false alarms on very small drives. More conservative = safer. |
| D9 | Stale files ledger instead of `--resync` on filter change | Some approaches require a destructive `--resync` (delete entire DB) when filter rules change. This is a sledgehammer approach that loses all sync state. Our stale files ledger tracks exactly which files are affected, lets the user decide what to do, and preserves all other sync state. |
| D10 | Timestamp-based conflict naming instead of hostname-based | Timestamps are self-documenting (you can see when the conflict occurred by looking at the filename). Hostnames are meaningless when the same user syncs from one machine. The timestamp is also shorter and more predictable. |
| D11 | Per-side hash baselines handle SharePoint enrichment | Per-side hash baselines handle enrichment naturally; no enrichment-specific code path needed. After upload, `LocalHash` records what is on disk and `QuickXorHash` records the server's enriched hash. The three-way merge compares each side against its own baseline, so divergent hashes do not trigger spurious actions. See [SHAREPOINT_ENRICHMENT.md](../../SHAREPOINT_ENRICHMENT.md). |
| D12 | Global token bucket for rate limiting instead of per-worker | With 8 download workers and 8 upload workers all hitting the API independently, a rate limit response (429) causes all 16 workers to back off and then retry simultaneously — the thundering herd problem. A shared token bucket coordinates all workers, spreading requests smoothly. |
| D13 | Blocking I/O for watch mode instead of polling | 100% CPU when idle is commonly caused by busy-loop polling. Using Go's `select` on channels backed by OS-level filesystem events (inotify/FSEvents) and network I/O means zero CPU when idle. |
| D14 | Parallel initial enumeration via `/children` walkers | The delta API returns items sequentially. For initial sync of large drives, parallel `/children` walks with 8 goroutines can enumerate the tree much faster by exploiting API-level concurrency. |
| D15 | Batch size of 500 items with WAL checkpoints | Without checkpointing, the WAL file can grow to hundreds of MB during initial sync of large drives. 500 items is small enough to checkpoint frequently but large enough to amortize transaction overhead. |
| D16 | Filter cascade (sync_paths → config → .odignore) with monotonic exclusion | Each layer can only exclude more, never include back. This makes filter behavior predictable and debuggable. If a file is excluded by any layer, it stays excluded regardless of what subsequent layers say. |
| D17 | Synced base uses server hash after transfers; per-side baselines are primary | After a download, we use the verified hash (which matches the server hash) as the synced base. After an upload, we use the server's response hash (which may differ due to enrichment). `SyncedHash` records the server's response hash for diagnostics and conflict context, but it is **not** the primary merge baseline. The three-way merge uses per-side baselines: `LocalHash` for local change detection, `QuickXorHash` for remote change detection. `SyncedHash` is secondary — it serves as an enrichment indicator and conflict resolution aid. See [SHAREPOINT_ENRICHMENT.md §4.6](../../SHAREPOINT_ENRICHMENT.md). |
