# Reference Implementation: Sync Algorithm Analysis

This document is a clean-room behavioral analysis of the reference OneDrive sync client's synchronization algorithm. It describes observable behaviors, design decisions, edge cases, and known problems -- not source code. It is intended for consumption by Tier 2 design agents who must never see the original implementation.

All terminology follows the [Domain Glossary](domain-glossary.md).

---

## Table of Contents

1. [Sync Modes](#1-sync-modes)
2. [Overall Sync Flow](#2-overall-sync-flow)
3. [Delta Processing](#3-delta-processing)
4. [Local Filesystem Scanning](#4-local-filesystem-scanning)
5. [State Management](#5-state-management)
6. [Upload Process](#6-upload-process)
7. [Download Process](#7-download-process)
8. [Move and Rename Detection](#8-move-and-rename-detection)
9. [Directory Ordering and Creation](#9-directory-ordering-and-creation)
10. [Error Handling and Resilience](#10-error-handling-and-resilience)
11. [Concurrency Model](#11-concurrency-model)
12. [Special Cases and Edge Cases](#12-special-cases-and-edge-cases)

---

## 1. Sync Modes

The reference implementation supports several distinct operational modes that alter which phases of the sync algorithm execute and in what order.

### 1.1 Bidirectional Sync (Default)

The default mode treats the remote (OneDrive) as the source of truth. The algorithm:
1. Fetches remote changes first (delta query)
2. Applies them locally (downloads, deletes, moves)
3. Checks the database for consistency against the local filesystem
4. Scans for local changes and uploads them
5. Performs a second remote fetch as a "true-up" pass to catch any changes that arrived during local processing

This double-fetch pattern ensures that changes made on the server during the local scan phase are not missed until the next sync cycle.

### 1.2 Download-Only Mode

Only fetches remote changes and applies them locally. No local scanning or uploading occurs. The flow is:
1. Fetch remote changes (delta query)
2. Apply locally (downloads, deletes, moves)
3. Database consistency check (but only to clean up stale DB entries, not to trigger uploads)

### 1.3 Upload-Only Mode

Only scans the local filesystem and uploads new or changed files. No delta query is performed. The flow is:
1. Database consistency check
2. Local filesystem scan
3. Upload new and changed files

### 1.4 Local-First Mode

A variant of bidirectional sync that reverses the initial ordering to prioritize local changes:
1. Database consistency check
2. Local filesystem scan and upload
3. Fetch remote changes and apply locally

This avoids the situation where a remote change overwrites a local edit before the local edit has a chance to be uploaded.

### 1.5 Single-Directory Mode

Restricts sync to a single specified directory path (and its descendants). This mode cannot use the standard delta query because delta returns changes for the entire drive. Instead, it uses a "simulated delta" mechanism that enumerates the target directory tree via the `/children` API endpoint recursively. See [Section 3.5](#35-simulated-delta-responses) for details.

### 1.6 Monitor (Continuous) Mode

Uses filesystem notification (inotify on Linux) to watch for local changes in real-time, triggering immediate uploads, moves, and deletions. Periodically performs a full sync cycle (configurable interval) to catch remote changes. Monitor mode has its own move/rename handling path that differs from the batch sync path (see [Section 8](#8-move-and-rename-detection)).

**Known problem:** Monitor mode relies on inotify, which has inherent limitations -- it does not work across network filesystems, has watch descriptor limits, and can miss events under heavy load. The reference implementation does not have a fallback polling mechanism for when inotify is unavailable.

---

## 2. Overall Sync Flow

### 2.1 Single Sync Cycle (Default Bidirectional)

A complete sync cycle in the default mode proceeds through these phases:

**Phase 1 -- Remote Delta Fetch (First Pass)**
- Retrieve the stored delta token (if any) from the database
- Call the delta API, paging through all results via `nextLink` URLs
- Classify each item (root, deleted, file, folder, remote/shared, package)
- Apply client-side filtering rules
- Process items: create directories immediately, queue file downloads, execute deletes and moves
- Store the new delta token from the final `deltaLink`

**Phase 2 -- Download Execution**
- Process all queued deletions first (local files/folders that were deleted remotely)
- Download all queued files, using a thread pool for parallelism
- Validate each download (size and hash)

**Phase 3 -- Shared Folder Processing**
- Iterate over all configured shared folders (both Personal and Business accounts)
- For each shared folder, perform its own delta fetch and download cycle
- Business shared folders use a simulated delta; Personal shared folders also use simulated delta

**Phase 4 -- Database Consistency Check**
- Walk every item in the database
- For each item, verify the corresponding local file/directory still exists
- If a local file is missing, flag the online copy for deletion (unless in download-only mode)
- If a local file has been modified (different hash), flag it for upload
- Execute flagged online deletions, then execute flagged uploads

**Phase 5 -- Local Filesystem Scan**
- Recursively walk the local sync directory
- Compare discovered items against the database
- New directories are queued for online creation
- New files are queued for upload
- Execute: create directories online first, then upload files

**Phase 6 -- Remote Delta Fetch (True-Up Pass)**
- Identical to Phase 1, using the delta token saved at the end of Phase 1
- Catches any remote changes that occurred during Phases 2-5
- Only occurs in the default bidirectional mode

### 2.2 Phase Ordering Variations

| Mode | Phase Order |
|------|-------------|
| Default (bidirectional) | 1 -> 2 -> 3 -> 4 -> 5 -> 1 (true-up) -> 2 -> 3 |
| Download-only | 1 -> 2 -> 3 -> 4 |
| Upload-only | 4 -> 5 |
| Local-first | 4 -> 5 -> 1 -> 2 -> 3 |

### 2.3 Dry-Run Mode

A cross-cutting option that prevents any write operations (local or remote). The algorithm runs normally but skips actual file creation, deletion, download, upload, and move operations. Database writes are also suppressed. Useful for previewing what a sync would do.

---

## 3. Delta Processing

### 3.1 Delta Query Mechanics

The delta API (`/drives/{driveId}/root/delta`) returns a stream of changed items since the last sync point, identified by a delta token. The response is paginated:

- Each page contains a batch of changed items plus either a `nextLink` (more pages) or a `deltaLink` (last page)
- The client must follow all `nextLink` URLs sequentially until a `deltaLink` is received
- The delta token embedded in the `deltaLink` is saved for the next sync cycle
- On the very first sync (no token), the delta API returns every item in the drive -- this is the "initial enumeration"

### 3.2 Item Classification

Each item in the delta response is classified into one of these categories for processing:

1. **Root item** -- Has the `root` facet. Upserted into the database immediately. No local action needed.
2. **Deleted item** -- Has the `deleted` facet. Queued for local deletion.
3. **File** -- Has the `file` facet. Either queued for download (new) or evaluated for changes (existing).
4. **Folder** -- Has the `folder` facet. Created locally immediately or evaluated for moves/renames.
5. **Remote item** -- Has the `remoteItem` facet. Represents a shared folder shortcut. A tie record is created in the database linking the local stub to the remote drive.
6. **Package** -- Has the `package` facet (e.g., OneNote notebooks). Explicitly skipped entirely.
7. **Unknown** -- None of the above facets. Logged and skipped.

**Critical ordering requirement:** Items within a delta response must be processed in the order they are received. The API may return the same item multiple times in a single response; the last occurrence represents the most recent state. Processing out of order can cause incorrect behavior (e.g., creating a file before its parent directory).

### 3.3 Client-Side Filtering

After classification, each item passes through a filtering cascade. If any filter matches, the item is skipped and its ID is added to a "skipped items" set. Any child items whose parent ID is in the skipped set are also automatically skipped (cascading exclusion).

The filtering rules, applied in order:
1. **skip_dir** -- Regex patterns matching directory names to exclude
2. **skip_file** -- Regex patterns matching file names to exclude
3. **sync_list** -- If configured, only items whose paths match the inclusion list are processed
4. **skip_dotfiles** -- If enabled, items whose names begin with `.` are excluded
5. **check_nosync** -- If enabled, directories containing a `.nosync` marker file are excluded
6. **skip_size** -- Files exceeding a configured size threshold are excluded

**Important behavior:** Filtering is only applied during real delta processing. When using simulated delta responses, the API calls themselves are pre-filtered (e.g., single-directory mode only queries the target subtree), so the filtering cascade is partially or fully bypassed. This means some filtering rules may behave differently in single-directory mode versus full sync.

**Known problem:** Items already present in the database from a previous sync are not re-evaluated against filtering rules during local filesystem scanning. If a user changes their filter configuration, previously synced items that now match an exclusion rule will not be cleaned up automatically.

### 3.4 Batch Processing

Delta items are not all processed at once. After collection from the API, they are processed in batches of 500 items. After each batch, a database checkpoint is performed (WAL checkpoint) to prevent the write-ahead log from growing unboundedly during large initial enumerations.

If the total number of items exceeds 300,000, a warning is issued referencing Microsoft's documented recommendation against tracking very large item sets with delta queries.

### 3.5 Simulated Delta Responses

The standard delta API cannot be used in several scenarios. In these cases, the reference implementation "simulates" a delta response by walking the online directory tree using `/children` API calls:

**Scenarios requiring simulated delta:**
- **Single-directory mode** -- Delta returns drive-wide changes; the client needs only one subtree
- **National cloud deployments** -- Some national clouds have incomplete or buggy delta API support
- **Cleanup-local-files mode** -- Needs to compare full online state against local state
- **Shared folders** -- Shared folder contents live on a different drive; the user's delta token only tracks their own drive

**Simulated delta algorithm:**
1. Before walking, all database items under the target scope have their sync status flag downgraded from "synced" to "not synced"
2. The online tree is walked recursively via `/children` API calls
3. Each discovered item is processed as if it came from a delta response, and its sync status is flipped back to "synced"
4. After the walk completes, any items still marked "not synced" are presumed deleted online and are removed locally

**Known problems with simulated delta:**
- It is significantly slower than real delta because it must enumerate the entire subtree every cycle, rather than receiving only changes
- It generates much more API traffic and is more likely to hit rate limits
- It does not benefit from delta's deduplication guarantees
- The sync status flag mechanism adds complexity to the database schema

### 3.6 Delta Token Lifecycle

- **First sync:** No token. Delta returns all items (initial enumeration).
- **Subsequent syncs:** Token from previous `deltaLink` is used. Delta returns only changes.
- **Token invalidation:** Server returns HTTP 410 Gone. Client discards the token and performs a fresh initial enumeration. The 410 response may include a resync type hint (`resyncChangesApplyDifferences` or `resyncChangesUploadDifferences`) but the reference implementation does not differentiate between these -- it always performs a full re-enumeration.
- **"Latest" token:** Calling delta with `?token=latest` returns an empty result and a fresh deltaLink. Used when the client wants to start tracking from "now" without enumerating existing items.
- **Token storage:** The delta token is stored in the item database, keyed by drive ID. A per-drive in-memory cache avoids redundant database reads within a single sync cycle.

**Known problem:** The reference implementation does not distinguish between the two resync hint types. A more sophisticated client could use `resyncChangesApplyDifferences` to trust the server state and `resyncChangesUploadDifferences` to prefer local state, but the reference implementation treats both identically.

---

## 4. Local Filesystem Scanning

### 4.1 Recursive Directory Walk

The local scan walks the sync root directory recursively using shallow directory enumeration (listing one level at a time, then recursing into subdirectories). This is a depth-first traversal.

For each discovered entry:
1. Check if it already exists in the database (by parent ID + name lookup)
2. If it exists in the database, skip it -- it was already handled during delta processing or consistency checking
3. If it does not exist in the database, it is a new local item

### 4.2 New Item Collection

New items are separated into two queues:
- **New directories** -- Added to a "paths to create online" list
- **New files** -- Added to a "files to upload" list

These queues are processed in order: all directories are created online first, then all files are uploaded. This ensures parent directories exist before their children are uploaded.

### 4.3 Path and Name Validation

Several validations are applied to discovered paths:

- **Path length:** Maximum 430 characters for Personal accounts, 400 for Business accounts. Items exceeding this are skipped with a warning.
- **UTF-8 validity:** File names are checked for valid UTF-8 encoding via grapheme cluster walking. Invalid names are skipped.
- **Unsupported characters:** OneDrive does not support certain characters in file names that are valid on POSIX filesystems. These are detected and the items are skipped.

### 4.4 Filtering During Local Scan

The same filtering rules (skip_dir, skip_file, sync_list, skip_dotfiles, check_nosync, skip_size) are applied to locally discovered items. However, there is an important subtlety with `sync_list`: even if a directory itself is excluded, it may still be traversed if the sync_list contains inclusion rules for paths beneath it. For example, if sync_list includes `/A/B/C`, directory `/A` and `/A/B` would normally be excluded, but they must be traversed to reach `/A/B/C`.

**Known problem:** The "traverse excluded directories to reach included subdirectories" logic adds significant complexity and can lead to confusing behavior where a directory appears to be partially synced.

---

## 5. State Management

### 5.1 Database Schema

The local state is maintained in a SQLite database using WAL (Write-Ahead Logging) journal mode with EXCLUSIVE locking and FULL synchronous writes. The schema centers on a single `item` table with the following fields:

| Field | Purpose |
|-------|---------|
| driveId | Drive containing this item (part of primary key) |
| id | Item ID within the drive (part of primary key) |
| name | Display name of the item |
| remoteName | Name on the remote drive (for shared items on Business; may differ from local name) |
| type | Item type enum: file, dir, remote, root, unknown |
| eTag | Entity tag for optimistic concurrency |
| cTag | Content tag for content change detection |
| mtime | Last modified time (from fileSystemInfo) |
| parentId | ID of parent item (foreign key with CASCADE DELETE) |
| quickXorHash | QuickXorHash of file content |
| sha256Hash | SHA256 hash of file content (Business only) |
| remoteDriveId | Drive ID of the source drive (for remote/shared items) |
| remoteParentId | Parent ID on the remote drive |
| remoteId | Item ID on the remote drive |
| remoteType | Type of the remote item |
| syncStatus | Single character flag: 'Y' (synced) or 'N' (not synced) |
| size | File size in bytes |
| relocDriveId | Target drive ID during move operations |
| relocParentId | Target parent ID during move operations |
| deltaLink | Stored delta token (only on root items) |

Primary key: `(driveId, id)`. Foreign key: `parentId` references `id` with CASCADE DELETE, meaning deleting a parent automatically removes all descendants.

### 5.2 Path Reconstruction

Because delta responses do not include `parentReference.path`, the database must be able to reconstruct the full local path for any item. This is done by walking the parent chain: starting from an item, repeatedly looking up the parent by `parentId` until reaching the root item or a remote item root.

For shared folders (remote items), path reconstruction uses the `relocDriveId` and `relocParentId` fields to bridge from the remote drive's item hierarchy back to the local mount point.

**Known problem:** Path reconstruction requires multiple database queries (one per ancestor level). For deeply nested files, this can be slow. The reference implementation does not cache reconstructed paths.

### 5.3 Sync Status Flag

The `syncStatus` field serves a dual purpose:
1. For normal delta processing: tracks whether an item has been successfully synced
2. For simulated delta processing: used as a "mark and sweep" mechanism to detect online deletions

During simulated delta:
- All items under the target scope are set to 'N' (not synced)
- As items are discovered online, they are set back to 'Y'
- Items still at 'N' after processing are presumed deleted online

### 5.4 Orphan Handling

When a new item arrives with an ID that differs from an existing database entry at the same path, the old entry is treated as an orphan. This can happen when OneDrive recreates an item (delete + create) rather than updating it in place. The old database entry is removed before the new one is inserted.

### 5.5 Database Consistency Check

The consistency check iterates over every item in the database and verifies it against the local filesystem:

**For files:**
- If the local file is missing: the item is flagged for deletion from OneDrive (unless in download-only mode, where the DB entry is simply removed)
- If the local file exists but has a different hash: the item is flagged for re-upload

**For directories:**
- If the local directory is missing: the item is flagged for deletion from OneDrive
- If the local directory exists: no action needed (directories have no content to verify)

**Known problem:** The consistency check reads every item from the database and checks the filesystem for each one. For large drives with hundreds of thousands of items, this is slow. There is no incremental consistency check -- it is always a full scan.

---

## 6. Upload Process

### 6.1 Upload Decision

Files are uploaded in two scenarios:
1. **New file** -- Discovered during local filesystem scan, not in the database
2. **Changed file** -- Exists in the database but local content differs (detected by hash comparison during consistency check)

### 6.2 Pre-Upload Checks

Before uploading, several checks are performed:

- **File size limit:** Maximum 250 GB. Files exceeding this are skipped.
- **Online quota:** The available quota is checked via the drive API. If insufficient, the upload is skipped. For shared folders, a more conservative "restricted" quota check is used.
- **Already exists online:** For new files, the client checks whether the file already exists at the target path online. If it does and content matches, only the database is updated (no upload needed). If it exists with different content, the conflict resolution strategy applies.
- **POSIX case sensitivity:** OneDrive is case-insensitive. Before uploading, the client checks whether a file with the same name but different casing already exists online. If so, the upload is skipped to avoid creating a confusing duplicate.

### 6.3 Simple Upload (Small Files)

Files of 4 MB or less are uploaded using a simple PUT request to `/drive/items/{parentId}:/{filename}:/content`. The entire file content is sent in a single request.

Zero-byte files always use simple upload regardless of any other consideration, because upload sessions require at least one non-empty chunk.

### 6.4 Upload Session (Large Files)

Files larger than 4 MB use a resumable upload session:

1. **Create session:** POST to `/drive/items/{parentId}:/{filename}:/createUploadSession` with conflict behavior and the desired `fileSystemInfo` timestamps
2. **Upload chunks:** Send file content in sequential chunks to the session URL. Each chunk response includes `nextExpectedRanges` indicating what to send next
3. **Session authentication:** The session URL includes embedded authentication -- no Bearer token is needed for chunk uploads
4. **Session lifetime:** Sessions expire after a period (typically a few days). The reference implementation does not persist session URLs across restarts, so interrupted uploads are lost

### 6.5 Modified File Upload

When uploading a changed version of an existing file:

1. Fetch the latest eTag from the server (to avoid 412 Precondition Failed from stale eTags)
2. Compare timestamps: if the online version is newer than the local version, the local file is backed up (safe backup) rather than overwriting the server version
3. For simple upload: include `If-Match` header with the latest eTag
4. For session upload: include the eTag in the session creation request

### 6.6 Post-Upload Validation

After a successful upload, the server response includes the new item metadata (including hashes). The client compares:
- **Hash:** quickXorHash (or SHA256 on Business) of the uploaded file against the server-reported hash
- **Size:** Local file size against the server-reported size

If validation fails, the upload is considered unsuccessful but the item is not deleted -- it remains online with a warning logged.

**Known problem:** Upload validation can fail spuriously for files that are modified locally during the upload process. The reference implementation does not lock files during upload.

### 6.7 Upload Ordering

Uploads are not ordered by default. When configured, they can be sorted by:
- File size (ascending or descending)
- File name (ascending or descending)

---

## 7. Download Process

### 7.1 Download Queue

Files to download are collected during delta processing. Each entry includes the item's JSON metadata and the computed local path. Downloads are not executed immediately during delta processing -- they are batched and executed after all delta items have been classified.

### 7.2 Pre-Download Checks

Before downloading each file:

- **Malware flag:** If the item's JSON includes a `malware` property, the download is skipped entirely with a warning. The local file (if any) is not modified.
- **Disk space:** Available disk space is checked against the file size plus a configurable reservation amount. If insufficient, the download is skipped.
- **Existing local file:** If a file already exists at the target path:
  - Compare its hash against the database record (not the incoming item -- this detects local modifications)
  - If the local file has been modified since last sync, create a safe backup before overwriting
  - If the local file matches the incoming item's hash, skip the download (content already correct)

### 7.3 Download Execution

Files are downloaded via the `@microsoft.graph.downloadUrl` pre-authenticated URL from the item's JSON metadata. This URL is short-lived (~1 hour) and does not require an Authorization header.

The download is written to a temporary location first, then moved to the final path after validation. (In practice, the reference implementation downloads directly to the target path, which is a known weakness -- a failed download can leave a corrupted file.)

**Known problem:** The reference implementation downloads directly to the target path rather than to a temporary file. If the download fails mid-stream or validation fails, the local file may be left in a corrupted state. The original file content is lost if no safe backup was created.

### 7.4 Post-Download Validation

After downloading, the client validates:
- **Size:** The downloaded file size must match the server-reported size
- **Hash:** The quickXorHash (or SHA256 on Business) of the downloaded content must match the server-reported hash

If validation fails:
- The downloaded file is deleted locally
- The database record for the item is purged
- The item will be re-downloaded on the next sync cycle

A configuration option (`--disable-download-validation`) can skip this validation, but it is not recommended.

**Known problems:**
- **HEIC files:** A known Microsoft API bug causes some `.heic` (iPhone photo) files to be reported with incorrect sizes. The reference implementation has special handling to not fail validation for these files based solely on size mismatch.
- **SharePoint size inconsistencies:** SharePoint may report different file sizes than the actual content. The reference implementation logs warnings but does not fail the download.

### 7.5 Timestamp Restoration

After a successful download, the local file's modification time is set to match the `fileSystemInfo.lastModifiedDateTime` from the item's JSON metadata. This preserves the original file timestamp rather than using the download time.

**Known problem:** If the server returns an invalid or missing timestamp, the reference implementation falls back to the current time, which can cause unnecessary re-syncs on the next cycle.

### 7.6 Download Ordering

Like uploads, downloads can optionally be sorted by size or name (ascending or descending).

### 7.7 Deletion Processing

Deletions are processed before downloads within the download execution phase. This ordering is important: it frees up disk space before new downloads consume it, and it avoids conflicts where a download would target a path that is about to be deleted.

Deletion behavior:
- **Files:** Removed from the local filesystem and from the database
- **Folders:** Only removed locally if they are empty after all other operations in the current batch. This prevents data loss when a folder is "deleted" but some of its children were moved rather than deleted.
- **No remote delete option:** A configuration flag (`--no-remote-delete`) prevents local deletions triggered by remote deletions. Items are removed from the database but the local files are preserved.

---

## 8. Move and Rename Detection

### 8.1 During Delta Processing (Batch Sync)

Move and rename detection during batch sync relies on comparing the incoming delta item against the existing database record:

**Detection criteria:**
- The item's eTag has changed (indicating server-side modification)
- AND the item's computed local path differs from the database's stored path

If both conditions are true, the item is treated as a move or rename.

**Execution:**
1. Check if the destination path is already occupied by a different local file
2. If occupied, create a safe backup of the existing file at the destination
3. Perform the local rename/move operation
4. Update the local file's timestamps to match the online item
5. Update the database record

**Known problem:** This detection mechanism cannot distinguish between "item was moved" and "item was modified AND its parent was renamed." Both produce the same signal (eTag changed + path changed). The reference implementation handles both cases the same way (local move), which is correct but may produce unnecessary file I/O if only the parent name changed.

### 8.2 During Monitor Mode (Real-Time)

In monitor mode, the filesystem notification system (inotify) directly reports move events with source and destination paths. This is more reliable than the delta-based detection.

**Execution:**
1. Look up the source path in the database to find the item's online identity
2. If not found in the database, treat the destination as a new file (upload it)
3. Check whether the move crosses drive boundaries (e.g., from the user's drive to a shared folder's drive)
4. **Same-drive move:** Send a PATCH request to the server updating the item's name and/or parent reference
5. **Cross-drive move:** Delete the item from the source drive, upload it to the destination drive (OneDrive does not support cross-drive moves)
6. Update the database

**Retry logic for moves:**
- On HTTP 412 (Precondition Failed): The eTag is stale. Clear the eTag and retry (up to 3 times)
- On HTTP 409 (Conflict): An item with the same name already exists at the destination. Delete the conflicting item first, then retry the move

### 8.3 Directory Move/Rename Online

When a local directory is moved or renamed, the client sends a PATCH request with the new name and parent reference. Before doing so:
1. Verify the source directory still exists online (it may have been independently deleted)
2. Verify the destination path does not already exist online (to avoid conflicts)
3. If the destination exists, the move is aborted with a warning

**Known problem:** The reference implementation does not handle the case where a directory is moved online and locally simultaneously. This can result in duplicate directories.

---

## 9. Directory Ordering and Creation

### 9.1 During Delta Processing

Directories discovered in delta responses are created locally **immediately** as they are processed, not batched. This is critical because:
- Delta items must be processed in order
- A file item may appear after its parent directory item in the same response
- If directory creation were deferred, the file download would fail due to a missing parent

Directory creation uses recursive creation (`mkdirRecurse` equivalent) -- if any intermediate directories are missing, they are created automatically.

### 9.2 During Local Scan (Upload Direction)

New local directories that need to be created online are collected into a queue and processed **before** new file uploads begin. This ensures that parent directories exist on the server before their children are uploaded.

**Processing order:** Directories are created in discovery order (depth-first), which naturally ensures parents are created before children.

### 9.3 Directory Deletion Ordering

Directory deletions follow the opposite order -- children must be processed before parents. The reference implementation handles this by:
1. Processing all non-directory deletions first
2. Only deleting a directory locally when it is confirmed empty
3. The CASCADE DELETE foreign key in the database automatically removes child records when a parent is deleted

**Known problem:** If a delta response includes a folder deletion but some of the folder's children were moved (not deleted), the folder deletion must wait until the moves are processed. The reference implementation handles this by checking emptiness, but the logic is fragile -- if a child item's move is processed in a later batch, the folder may be deleted prematurely.

---

## 10. Error Handling and Resilience

### 10.1 HTTP Error Handling

| Status Code | Behavior |
|-------------|----------|
| 408 (Timeout) | Retry with backoff (handled in API layer) |
| 410 (Gone) | Delta token invalidated. Discard token, perform full re-enumeration |
| 412 (Precondition Failed) | eTag mismatch. Refresh eTag and retry |
| 423 (Locked) | File checked out by another user (SharePoint). Skip with warning |
| 429 (Too Many Requests) | Wait for `Retry-After` duration, then retry |
| 403 (Forbidden) | On shared items: read-only permission. Skip with warning. Otherwise: log error |
| 500, 502, 503, 504 | Server errors. Retry with backoff |
| 509 (Bandwidth Exceeded) | SharePoint-specific. Treated similarly to 429 |

### 10.2 Delta Token Invalidation

When the server returns HTTP 410 for a delta request:
1. Log a warning indicating the token has expired
2. Discard the stored delta token
3. Retry the delta request with no token (triggering a full initial enumeration)

The 410 response may include a resync hint, but the reference implementation does not differentiate between resync types. Both are handled identically with a full re-enumeration.

**Known problem:** A full re-enumeration after token invalidation is expensive for large drives. A smarter implementation could use the resync hint to optimize: `resyncChangesApplyDifferences` means "trust the server," while `resyncChangesUploadDifferences` means "upload what the server doesn't know about." The reference implementation misses this optimization.

### 10.3 Safe Backup (Conflict Resolution)

When a conflict is detected (e.g., a local file was modified while the server version was also modified), the reference implementation creates a "safe backup" of the local file:

- The local file is renamed with a pattern incorporating the hostname (e.g., `filename-hostname-safeBackup.ext`)
- The server version is then downloaded to the original filename
- The backed-up file will be detected as a new local file on the next scan and uploaded

A configuration option (`--bypass-data-preservation`) disables safe backups, causing the server version to silently overwrite local changes.

**Known problem:** Safe backups can accumulate if conflicts recur. There is no automatic cleanup mechanism. Users may end up with many `*-safeBackup*` files.

### 10.4 Filesystem Errors

- **Permission denied:** Logged as a warning; the item is skipped
- **Disk full:** Checked before each download; download is skipped if insufficient space
- **Invalid characters in filename:** OneDrive allows some characters that POSIX does not, and vice versa. Invalid names are skipped with warnings
- **Path too long:** Paths exceeding OS or OneDrive limits are skipped

### 10.5 Network Resilience

The reference implementation does not have explicit offline detection or queuing. If network requests fail:
- Transient errors (timeouts, 5xx) are retried with backoff
- Persistent failures cause the current sync cycle to abort
- The next sync cycle (in monitor mode) or the next invocation (in single-run mode) will retry from the last saved delta token

---

## 11. Concurrency Model

### 11.1 Thread Pool

The reference implementation uses a thread pool for parallel downloads and uploads. The pool size is configurable (defaulting to a reasonable number based on available cores).

**Thread pool usage:**
- **Downloads:** Files are divided into batches sized by the thread count. Each batch is processed in parallel, with each thread operating on one file.
- **Uploads (changed files):** Same batching and parallel execution as downloads.
- **Uploads (new files):** Same batching and parallel execution.

Each thread in the pool gets its own API client instance (with its own HTTP connection/handle) to avoid contention on shared HTTP state.

### 11.2 Database Synchronization

All database operations are protected by a mutex. This ensures that parallel download/upload threads do not corrupt the database with concurrent writes. The mutex is per-operation (not per-transaction), so individual reads and writes are atomic but multi-step operations are not.

**Known problem:** The per-operation mutex means that a sequence like "read item, check condition, write update" is not atomic. Two threads could read the same item, both decide to update it, and the second write would silently overwrite the first. In practice, this is mitigated by the fact that each thread operates on different items, but the design does not guarantee this.

### 11.3 Sequential Operations

Despite the parallel download/upload capability, several operations are strictly sequential:

- **Delta item processing:** Items are processed one at a time in API response order. This is required for correctness (parent directories must be created before children).
- **Directory creation (online):** Directories are created one at a time to ensure proper ordering.
- **Deletion processing:** Deletions are processed sequentially.

### 11.4 API Rate Limit Awareness

The thread pool increases API traffic proportionally to the number of threads. Each thread independently handles 429 (rate limit) responses with its own retry/backoff logic. There is no global rate limiter coordinating across threads.

**Known problem:** Without a global rate limiter, a burst of parallel requests can trigger rate limiting, causing all threads to back off simultaneously and then retry simultaneously, creating a "thundering herd" pattern.

---

## 12. Special Cases and Edge Cases

### 12.1 OneNote Packages

OneNote notebooks are represented as DriveItems with the `package` facet (type "oneNote"). They appear as folders containing `.one` and `.onetoc2` files, but they must be treated as opaque units.

The reference implementation detects OneNote packages via multiple signals:
- The `package` facet with type "oneNote"
- MIME types: `application/msonenote` and `application/octet-stream`
- File extensions: `.one` and `.onetoc2`
- A folder named `OneNote_RecycleBin`

All OneNote items are unconditionally skipped. They are not synced locally at all.

**Rationale:** OneNote has its own sync mechanism. Attempting to sync notebook files at the file level would cause corruption.

### 12.2 Shared Folders (Personal vs. Business)

Shared folder handling differs significantly between account types:

**Personal accounts:**
- Shared folders appear as remote items in the user's drive
- The `remoteItem` facet contains the source drive ID and item ID
- Contents are synced by performing a simulated delta on the remote drive, scoped to the shared folder

**Business accounts:**
- Shared folders also appear as remote items
- The display name in the user's drive may differ from the actual name on the source drive (the `remoteName` field tracks this)
- Business shared folders that are individual files (not folders) get special handling: they are placed in a dedicated local directory

### 12.3 Personal Account DriveId Normalization

The OneDrive API for personal accounts sometimes returns drive IDs with inconsistent casing. The reference implementation normalizes all Personal account drive IDs to lowercase. Additionally, Personal account drive IDs are expected to be 16 characters long; if shorter, they are zero-padded on the left.

**This is a workaround for a known Microsoft API bug.**

### 12.4 Timestamp Handling

**Truncation:** All timestamp comparisons truncate to whole seconds. The API may return sub-second precision, but the local filesystem may not support it. Truncating both sides ensures consistent comparison.

**Invalid timestamps:** The API can return invalid or zero timestamps. The reference implementation detects these and substitutes the current time.

**Timestamp sync:** When a file's content hash matches but timestamps differ, the reference implementation resolves the mismatch:
- If the local file is newer and we are not in download-only/resync mode: update the online timestamp to match local
- Otherwise: update the local timestamp to match online

This prevents unnecessary re-syncs caused by timestamp drift.

### 12.5 Case Sensitivity

OneDrive is case-insensitive (on both Personal and Business), but Linux/macOS filesystems are typically case-sensitive. This creates potential conflicts:

- A local filesystem can have `File.txt` and `file.txt` in the same directory; OneDrive cannot
- Before uploading a new file, the reference implementation checks online for case-insensitive name collisions
- If a collision is found, the upload is skipped with a warning

**Known problem:** The case-sensitivity check only runs for new file uploads. It does not handle the case where two files with the same name (different casing) are both already synced -- this can happen if one was created online and the other locally.

### 12.6 Permanent Delete Option

By default, deleting items online sends them to the OneDrive recycle bin. A configuration option (`--remove-source-files` combined with other flags, or `--permanent-delete`) can permanently delete items, bypassing the recycle bin.

### 12.7 Local Delete After Upload

A special mode (`--local-delete-after-upload`) deletes local files after they are successfully uploaded. This is intended for scenarios where the local disk is used as a staging area and files should be moved to OneDrive, not kept locally.

### 12.8 Large File Handling

- Files larger than 250 GB cannot be uploaded (Microsoft's limit)
- Files larger than 4 MB use resumable upload sessions (with chunk-based upload)
- The reference implementation does not fragment downloads -- files are downloaded in a single HTTP request regardless of size

**Known problem:** Very large file downloads (multi-GB) are vulnerable to network interruptions because there is no resume capability for downloads. The download must restart from the beginning if interrupted.

### 12.9 Zero-Byte Files

Zero-byte files are a special case:
- They always use simple upload (upload sessions require at least one non-empty chunk)
- Hash verification is trivially satisfied (empty content always produces the same hash)
- They cannot be used for content-based conflict detection

### 12.10 Symbolic Links

The reference implementation does not follow or sync symbolic links. They are silently ignored during local filesystem scanning.

### 12.11 National Cloud Deployments

Different Microsoft national clouds (US Government, China/21Vianet, Germany) have different API endpoints and may have different feature availability. The reference implementation:
- Uses configurable endpoint URLs
- Falls back to simulated delta (instead of real delta) for national clouds due to unreliable delta support
- Uses the same sync algorithm otherwise

### 12.12 Quota Handling

Before uploading, the client checks the drive's available quota:
- If the remaining quota is less than the file size, the upload is skipped
- For shared folders on Business accounts, a "restricted" quota check uses a different threshold
- If the API reports "remaining" quota as zero but "total" quota is also zero, this indicates unlimited storage (common on some Business plans), and the upload proceeds

---

## Summary of Known Problems

For reference by Tier 2 design agents, these are the key problems and limitations identified in the reference implementation that a new client should address:

1. **No atomic file operations:** Downloads write directly to the target path, risking corruption on failure. Should use temp files with atomic rename.
2. **Full re-enumeration on token invalidation:** Does not use resync hints to optimize recovery. Should differentiate between `resyncChangesApplyDifferences` and `resyncChangesUploadDifferences`.
3. **No download resume:** Large file downloads cannot be resumed after interruption. Should implement range-based download resume.
4. **No global rate limiter:** Parallel threads independently handle rate limits, causing thundering herd. Should implement a shared rate limiter/token bucket.
5. **Simulated delta is slow:** The fallback for national clouds, single-directory, and shared folders enumerates everything every cycle. Should explore more efficient alternatives.
6. **Path reconstruction is expensive:** Walking the parent chain for every item requires O(depth) database queries. Should cache or materialize paths.
7. **Filter changes are not retroactive:** Changing filter rules does not clean up previously synced items. Should detect configuration changes and reconcile.
8. **Safe backup accumulation:** No cleanup mechanism for conflict backup files. Should implement a policy for aging out or notifying about old backups.
9. **No per-operation database transactions:** Multi-step database operations are not atomic. Should use transactions for read-modify-write sequences.
10. **Monitor mode inotify limitations:** No fallback polling for environments where inotify is unavailable or unreliable (network filesystems, watch limit exhaustion).
11. **Case sensitivity conflicts not fully handled:** Only checked for new uploads, not for existing synced items or renames.
12. **Upload session not persisted:** Interrupted large file uploads must restart from scratch on the next sync cycle. Should persist session URLs for resumption.
