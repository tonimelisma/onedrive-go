# Reference Implementation: Conflict Scenarios Analysis

This document analyzes how the reference OneDrive sync client detects, classifies, and resolves conflicts between local and remote state. It describes observable behaviors, strategies, and edge cases -- not code structure.

---

## 1. Conflict Detection

The reference implementation detects conflicts by comparing three signals between the local filesystem, the local database (the "last known synced state"), and the remote OneDrive state:

1. **Modification timestamps** -- compared between the local file's `mtime` and the database/remote item's `lastModifiedDateTime`. Both values are truncated to whole-second precision (sub-second fractional parts are zeroed) before comparison. This truncation is deliberate because different filesystems and APIs have different timestamp granularity, so fractional seconds would cause false positives.

2. **Content hashes** -- computed only when timestamps differ, as hashing is computationally expensive. The hash used depends on the account type (see Section 6).

3. **eTags** -- compared between the database-cached eTag and the eTag returned by delta/API queries to determine whether the remote item has changed at all (metadata or content).

The sync engine checks these signals at multiple points during a sync cycle:

- **During delta processing**: When processing items from the `/delta` API response, each item is checked against the local database. Items where the eTag has changed are flagged as "changed items" requiring further analysis.
- **During database consistency checks**: After processing delta responses, the engine walks the local database and compares each entry against the actual local filesystem state. Files where the local `mtime` differs from the database `mtime` are candidates for upload.
- **During new-file upload**: Before uploading a file that appears to be "new," the engine queries OneDrive to check whether a file with that name already exists at the target path. If one does, a conflict scenario is triggered.

### The `isItemSynced` Function

The central mechanism for determining whether a local file matches its expected state is a sync-check that works as follows:

1. If the file does not exist locally, it is not in sync.
2. If the file cannot be read (permissions or corruption), it is not in sync.
3. If the local `mtime` (truncated to seconds) equals the database item's `mtime`, the item is considered in sync. This is the fast path.
4. If timestamps differ, a content hash is computed. If the hash matches, the item is considered "same content but wrong timestamp" and the appropriate timestamp is corrected (locally or remotely, depending on operational mode). Despite fixing the timestamp, the function returns `false` (not in sync) so that the caller can take the corrected state into account.
5. If the hash also differs, the item is genuinely out of sync.

For directories, the sync check always returns `true` -- directory conflicts are not tracked by content comparison, only by existence and name.

---

## 2. Conflict Types

### 2.1 Edit-Edit Conflict (File Modified Both Locally and Remotely)

This is the classic conflict. It occurs when:
- The file existed and was synced (present in the database).
- The remote version has changed (detected via delta, eTag/hash difference).
- The local version has also changed (local `mtime` differs from database `mtime`, and the local hash differs from the database hash).

Detection path: During delta processing, when downloading a changed remote file, the engine checks whether the local file has been modified since the last sync by comparing the local file's hash against the database record's hash. If they differ, the local file has been modified and a conflict exists.

### 2.2 File Modified Locally, Deleted Remotely

This occurs when:
- The delta response indicates an item has been deleted (the `deleted` facet is present).
- The item exists in the local database.
- The local file exists on disk but its hash differs from the database record's hash.

The engine explicitly cannot use the hash from the deleted item's API response, because Microsoft returns invalid/zeroed-out hashes for deleted items (e.g., `quickXorHash` of `"AAAAAAAAAAAAAAAAAAAAAAAAAAA="`). Instead, the engine compares the local file's hash against the database record -- which represents what was last known to be in sync.

### 2.3 File Deleted Locally, Modified Remotely

This is handled implicitly during the database consistency check phase. When the engine walks the database entries and finds that a file referenced in the database no longer exists on the local filesystem, the item is flagged for re-download from OneDrive if the remote version still exists. The local deletion is effectively reversed -- the remote version is downloaded.

Conversely, if the engine is not in `--download-only` mode, a local deletion is interpreted as an intentional delete and the engine will propagate the deletion to OneDrive (delete the remote file). This is the normal delete-propagation path, not a conflict per se.

### 2.4 File Created Locally and Remotely with Same Name

When uploading a new local file, the engine first queries OneDrive to see whether a file with the same name already exists at the target path. If it does:

1. The engine compares the local file's content (hash and size) against the online file.
2. If the content is identical, no upload is needed -- the online item's metadata is simply saved to the local database.
3. If the content differs, the engine compares modification timestamps to determine which is newer:
   - **Local is newer or same age**: The local file is uploaded as a "modified file" (replacing the online version).
   - **Remote is newer**: The local file is renamed via `safeBackup`, the renamed copy is uploaded as a new file, and the original filename's database entry is removed so the newer remote version can be downloaded.

A POSIX case-sensitivity check is also performed: if the local filename is a case-insensitive match to an existing online item but differs in case (e.g., `File.txt` vs `file.txt`), the upload is rejected with an error instructing the user to rename the local file.

### 2.5 Directory Conflicts

Directories receive much simpler conflict handling than files:

- **Directory rename/move detected remotely**: When the eTag changes and the path computed from the delta response differs from the existing path in the database, the engine performs a local rename/move of the directory. If the destination path already exists, the occupying item is backed up via `safeBackup` (though `safeBackup` skips directories -- it only renames files to avoid data loss).
- **Directory deleted remotely**: The engine processes deletions in reverse order (children before parents) to ensure directories are empty before attempting removal.
- **Directory deleted locally**: If a directory tracked in the database no longer exists locally, and the engine is not in `--download-only` mode, the deletion is propagated online.

Directories are never hash-compared. The `isItemSynced` function always returns `true` for directory-type items, meaning directory "conflicts" are resolved purely by existence and name matching.

### 2.6 Remote Item (Shared Folder/File) Conflicts

Shared items (items from other users' drives) receive their own conflict handling:

- **Shared files (OneDrive for Business)**: When syncing shared files, if a file with the same name exists locally and is not in sync, the engine checks whether a database record exists. If one exists, the item is processed as a "potentially changed item." If no database record exists, the local file is backed up via `safeBackup` and the remote shared file is downloaded.
- **Shared folder structure**: Shared files are stored in a special local directory (`Files Shared With Me/<owner>/<shared-folder-name>/`). This entire directory structure is excluded from being synced back to OneDrive to prevent accidentally uploading shared-file backups to folders where the user may lack write permissions.
- **Read-only shared files**: If an upload of a modified shared file fails with HTTP 403, the engine logs that the file was shared as read-only and skips the upload.

### 2.7 Move Conflicts

When the engine detects that an item has been moved or renamed remotely (eTag changed and computed path differs from existing path):

- **Destination occupied by a synced file**: If the destination path is already occupied by a file that is currently in sync with OneDrive (per `isItemSynced`), the destination file will be overwritten.
- **Destination occupied by an out-of-sync file**: The occupying file is backed up via `safeBackup` before the move proceeds.
- **Destination occupied by an un-tracked file** (not in the database at all): The occupying file is backed up via `safeBackup` before the move proceeds.

When the engine propagates a local move/rename to OneDrive:
- The engine uses the eTag in an `If-Match` header for optimistic concurrency.
- If the server returns HTTP 412 (Precondition Failed), the engine retries without the eTag (up to 3 attempts).
- If the server returns HTTP 409 (Conflict -- destination already exists online), the engine deletes the existing online item at the destination first, then retries the move.

### 2.8 Rename Conflicts

Renames are handled identically to moves because the OneDrive API treats both as the same operation (updating the `name` and/or `parentReference`). The same eTag-based optimistic concurrency and `safeBackup` logic described in Section 2.7 applies.

### 2.9 Type Change Conflicts (File to Folder or Vice Versa)

The reference implementation does not appear to have explicit handling for the scenario where a path changes from being a file to a folder (or vice versa) between sync cycles. In practice, such a scenario would manifest as:
- A delta response showing a deletion of the old item type and creation of a new item with the new type.
- The deletion would be processed first (removing the local file or folder), and then the creation would proceed (creating the new folder or downloading the new file).

There is no single-step "type change" detection; it is decomposed into separate delete and create operations by the delta response itself.

---

## 3. Conflict Resolution Strategies

### 3.1 General Philosophy: Remote Wins, Local is Preserved

The overarching strategy is: **the remote (OneDrive) version is treated as authoritative, and the local version is preserved by renaming.** The engine never silently deletes or overwrites a locally modified file without first creating a backup copy. This matches how the official Microsoft Windows OneDrive client behaves.

The only exception to "remote wins" is the timestamp-based tiebreaker during upload conflicts (Section 2.4), where a locally-modified file that is newer than the online version will be uploaded, overwriting the remote copy.

### 3.2 Resolution by Conflict Type

| Conflict Type | Resolution |
|---|---|
| Edit-edit (file modified both sides) | Remote version is downloaded. Local version is renamed via `safeBackup`. The renamed local file is then uploaded as a new file in the next scan. |
| File modified locally, deleted remotely | If local hash differs from database hash: local file is renamed via `safeBackup`, database record is purged, renamed file will be uploaded as new. If hash matches database: local file is deleted. |
| File deleted locally, modified remotely | Remote version is downloaded (local deletion is reversed). |
| Same-name creation (local newer) | Local file replaces remote version via upload. |
| Same-name creation (remote newer) | Local file is renamed via `safeBackup`, renamed copy is uploaded, remote version is downloaded to original filename. |
| Move/rename destination occupied | Occupying file is backed up via `safeBackup` if it is not in sync; overwritten if it is in sync. |
| Online move fails (409) | Existing online destination item is deleted first, then move is retried. |
| Online move fails (412) | eTag is cleared (set to null) and the move is retried without optimistic concurrency. Up to 3 attempts. |

### 3.3 The `--local-first` Mode

When the `--local-first` flag is used, the engine treats the local filesystem as the source of truth for what should exist online. However, this does **not** change how edit-edit conflicts are resolved. Microsoft OneDrive has no concept of "local-first," so conflict handling still aligns with how the official OneDrive client works: the local file is renamed, and the remote version takes the original filename.

The practical difference with `--local-first` is in the processing order:
1. The engine first scans the local filesystem for changes and uploads them.
2. Then it fetches the delta from OneDrive and processes remote changes.
3. If both sides changed the same file, the local file was already uploaded (possibly overwriting the remote version if the local timestamp was newer). If the remote was newer, the local file is renamed and the remote version is downloaded.

### 3.4 The `--resync` Mode

When `--resync` is used, the entire local database is deleted before syncing. This means the engine has **zero** knowledge of what was previously in sync. Consequently:
- The online state is always treated as the source of truth.
- Any local file whose hash differs from the online version is renamed via `safeBackup`, and the online version is downloaded.
- This is true regardless of whether `--local-first` is also being used.

### 3.5 The `bypass_data_preservation` Configuration

When `bypass_data_preservation = "true"` is set in the configuration, the `safeBackup` function is effectively disabled for most conflict scenarios. The function returns immediately without renaming the local file. This means:
- In edit-edit conflicts, the local file is simply overwritten by the remote version.
- In modified-locally/deleted-remotely conflicts, the local file is deleted without a backup.
- This can cause **local data loss** and the engine logs an explicit warning at startup.

There are specific conflict scenarios where `safeBackup` is called with `bypassDataPreservation` forced to `false` regardless of the configuration setting. This occurs during upload conflict resolution when the online file is newer than the local file -- the local file is **always** renamed in this case, because both versions need to coexist (the renamed local is uploaded as a new file, and the original filename is freed for the remote version to be downloaded).

---

## 4. eTag and cTag Role in Conflicts

### 4.1 eTag for Change Detection

The eTag is the primary signal used to detect that a remote item has changed at all. During delta processing, the engine compares the eTag from the API response against the eTag stored in the local database:

- **eTags match**: The item has not changed. No further processing is needed (with caveats about simulated delta responses).
- **eTags differ**: The item has changed in some way. The engine then examines what changed:
  - Is the path different? (move/rename)
  - Is the content hash different? (content change requiring download)
  - Is only the timestamp different? (metadata-only change)

The engine explicitly acknowledges that "the eTag is notorious for being 'changed' online by some backend Microsoft process." To avoid unnecessary downloads when only the eTag changed but not the content, the engine compares `quickXorHash` values. If the hash is unchanged despite the eTag change, no download is performed.

### 4.2 eTag for Optimistic Concurrency

The eTag is sent in `If-Match` headers on update and delete API requests:

- **Timestamp updates**: When updating the `lastModifiedDateTime` on an online item, the engine passes the eTag. If the server returns 409 or 412, the engine retries without the eTag.
- **File moves**: When moving items online, the eTag is passed. On 412, the eTag is cleared and the operation is retried (up to 3 times).
- **Upload sessions**: When creating an upload session for a modified file, the engine fetches the latest eTag from OneDrive to minimize the chance of a 412 error.
- **Deletions**: The eTag is passed when deleting items online.

### 4.3 eTag Quirks and Workarounds

- **OneDrive Personal accounts**: For personal accounts, the eTag is explicitly nullified (set to `null`) before metadata update calls "to avoid 412 errors as much as possible." This suggests that personal account eTags are particularly unreliable for concurrency control.
- **Missing eTags**: The engine handles cases where the API response does not include an eTag. In such cases, it falls back to the database eTag value, with debug logging noting the "greater potential for a 412 error."
- **Upload sessions**: Before creating an upload session, the engine makes an additional API call to fetch the absolute latest item details, specifically to get a fresh eTag and minimize 412 failures.

### 4.4 cTag Role

The cTag (content tag) is stored in the database alongside the eTag, but it is **not** directly used for conflict detection logic. The engine relies on content hashes (`quickXorHash` or SHA256) rather than cTags for determining whether file content has changed. This is pragmatic because:
- cTags are not returned for folders on OneDrive for Business.
- cTags are not returned in delta query responses for create/modify operations on OneDrive for Business.
- cTags are not returned for deleted items on either account type.

The cTag is updated in the database when it appears in API responses, but it does not drive any conflict resolution decisions.

---

## 5. Timestamp-Based Detection

### 5.1 Timestamp Truncation

All timestamp comparisons truncate fractional seconds to zero (`fracSecs = Duration.zero`). This is done on both the local file's `mtime` (obtained from the filesystem) and the remote/database item's `mtime`. The truncation prevents false positives from platforms and filesystems with different timestamp precision (e.g., NTFS stores to 100ns, ext4 to 1ns, FAT32 to 2s, and the OneDrive API returns ISO 8601 strings with variable precision).

### 5.2 Timestamp as First-Pass Filter

Timestamps are used as the initial "fast path" check before resorting to expensive hash computation:
- If the local `mtime` equals the database `mtime` (after truncation), the file is assumed to be in sync without computing a hash.
- If the timestamps differ, a hash is computed to determine whether the content actually changed or if only the timestamp drifted (e.g., due to a file indexer touching the file).

### 5.3 Timestamp Correction

When the hash matches but the timestamp differs, the engine corrects the "wrong" timestamp:

- **Normal sync, local newer**: The online timestamp is updated to match the local timestamp (the local file was presumably touched by an indexer but not modified).
- **Normal sync, local older**: The local timestamp is corrected to match the database/online timestamp.
- **`--resync` mode**: The local timestamp is always corrected to match the online timestamp, regardless of which is newer.
- **`--download-only` mode**: The local timestamp is always corrected to match the online timestamp.

### 5.4 Timestamp for Upload Conflict Resolution

When determining which version "wins" during an upload conflict (file exists both locally and remotely with different content):
- Timestamps are compared after truncation to seconds.
- `localModifiedTime >= onlineModifiedTime` means the local version wins and is uploaded.
- `localModifiedTime < onlineModifiedTime` means the remote version wins; the local file is renamed and the remote is downloaded.

### 5.5 Edge Cases

- **UTC normalization**: All local timestamps are converted to UTC (`.toUTC()`) before comparison with OneDrive timestamps (which are always UTC).
- **Invalid timestamps**: If a file no longer exists on disk when its timestamp is queried, the engine falls back to the current system time.
- **Timestamp drift between checks**: The engine acknowledges in comments that a file's local modification time can change between the delta check and the database consistency check (e.g., because the user edited the file during sync).

---

## 6. Hash Verification

### 6.1 Hash Types by Account

The engine supports multiple hash types with a clear preference order:

1. **QuickXorHash** -- preferred, checked first. Available on both Personal and Business accounts.
2. **SHA256** -- fallback for Business accounts if QuickXorHash is absent.
3. **SHA1** -- not used for conflict detection (only indirectly for internal operations like faking database entries).

The `testFileHash` function:
1. If the database item has a `quickXorHash`, compute the local file's QuickXorHash and compare.
2. Else if the database item has a `sha256Hash`, compute the local file's SHA256 hash and compare.
3. If neither hash exists in the database record, the function returns `false` (not matching), which triggers the "content is different" path.

### 6.2 When Hashing is Performed

Hashing is intentionally deferred because it is computationally expensive:

- **During `isItemSynced`**: Only if the timestamps differ. If timestamps match, the file is assumed in sync without hashing.
- **During database consistency checks**: Only if the local `mtime` differs from the database `mtime`.
- **During download conflict checks**: Before overwriting a local file with a remote version, the engine hashes the local file against the database record to detect local modifications.
- **After download**: The downloaded file's hash is computed and compared against the hash reported by the API to verify download integrity.

### 6.3 Download Integrity Verification

After downloading a file, the engine verifies integrity by comparing:
- The hash computed from the downloaded file against the hash reported by the OneDrive API.
- The file size on disk against the size reported by the API.

QuickXorHash incorporates the file length into the final digest, so a size mismatch would typically also produce a hash mismatch. However, QuickXorHash is not collision-resistant, so both checks are performed. If validation fails, the download is logged as an error.

This download validation can be disabled via `--disable-download-validation`.

### 6.4 AIP (Azure Information Protection) File Handling

For files protected by Azure Information Protection, the API-reported hash and size may differ from the actual on-disk values after download (the file is decrypted on download). The engine detects this scenario (hash and size both differ from API-reported values) and updates the JSON data with the actual local values before saving to the database. This prevents false hash mismatches on subsequent sync cycles.

### 6.5 Invalid Hashes from API for Deleted Items

When the API returns data for deleted items, the hash values are invalid (e.g., `quickXorHash` of `"AAAAAAAAAAAAAAAAAAAAAAAAAAA="`). The engine explicitly documents this and never uses the API-provided hash for deleted-item comparisons. Instead, it compares the local file's hash against the database record.

---

## 7. Safe Deletion

### 7.1 Hash-Before-Delete Guard

Before deleting a local file due to a remote deletion event, the engine performs a safety check:

1. Check whether the local file is "in sync" with the database record (via `isItemSynced`).
2. If in sync: proceed with deletion -- the local file matches what was known to be synced, so deleting it is safe.
3. If not in sync: compare the local file's hash against the **database** hash (not the API hash for the deleted item, which is invalid).
   - If the hashes match: the `isItemSynced` failure was due to a timestamp discrepancy only. Proceed with deletion.
   - If the hashes differ: the local file has been modified since the last sync. Back it up via `safeBackup` before allowing the delete path to proceed. The database record for the old item is purged, and the renamed backup file will appear as a new local file on the next scan.

### 7.2 Big Delete Protection

When propagating local deletions to OneDrive, the engine counts how many items would be deleted. If the count meets or exceeds a configurable threshold (`classify_as_big_delete`), the engine:
- Aborts the sync entirely to protect online data from accidental mass deletion.
- Logs an error message indicating how many items would have been deleted.
- Instructs the user to use `--force` or increase the threshold if the deletion is intentional.

This protects against scenarios like accidentally deleting the sync directory or a large subdirectory.

### 7.3 Reverse-Order Deletion

When processing local deletions (items that were deleted online), the engine iterates in reverse order. This ensures that child files are deleted before their parent directories, preventing "directory not empty" errors.

### 7.4 Recycle Bin Support

Two forms of recycle bin support exist:

- **Online recycle bin**: By default, when deleting items on OneDrive, they are sent to the online recycle bin (soft delete). If `permanent_delete = "true"` is configured, items are permanently deleted instead (only supported on certain account types and cloud deployments).
- **Local recycle bin**: If `use_recycle_bin` is configured, items that are deleted locally due to remote deletion events are moved to a local recycle bin directory (following the FreeDesktop.org Trash specification with `.trashinfo` metadata) rather than being permanently removed from the local filesystem.

### 7.5 Upload-Only Mode Protections

When `--upload-only` is combined with `--no-remote-delete`, the engine will not propagate local deletions to OneDrive. The engine logs a warning about this at startup.

---

## 8. Conflict File Naming

### 8.1 Naming Pattern

Conflict copies (backup files) use the following naming pattern:

```
<stem>-<hostname>-safeBackup-<counter><extension>
```

Where:
- `<stem>` is the original filename without extension.
- `<hostname>` is the machine's hostname (obtained via `Socket.hostName`).
- `-safeBackup-` is a fixed tag that identifies the file as a conflict backup.
- `<counter>` is a zero-padded 4-digit number starting at `0001`.
- `<extension>` is the original file extension (including the dot).

Example: `report.docx` becomes `report-myworkstation-safeBackup-0001.docx`.

### 8.2 Counter Increment for Existing Backups

If the file being backed up is itself already a `safeBackup` file (i.e., its stem already ends with `-<hostname>-safeBackup-NNNN`), the engine increments the 4-digit counter rather than appending another tag. This prevents names from growing unboundedly like `file-host-safeBackup-0001-host-safeBackup-0001.txt`.

The detection works by checking whether the stem ends with the tag pattern plus exactly 4 digits. If so, the counter is extracted, incremented, and used with the original base stem.

### 8.3 Collision Avoidance

Before settling on a candidate name, the engine checks whether the candidate path already exists on the local filesystem. If it does, the counter is incremented and a new candidate is generated. This loop continues up to 1000 attempts. If no unique name can be found after 1000 tries, the backup fails with an error log entry, and the original file is left in place (the engine does not overwrite it).

### 8.4 Directories Are Never Renamed

The `safeBackup` function explicitly checks whether the input path is a directory. If it is, the function logs a message and returns immediately without performing any rename. This means directory conflicts never result in renamed backup directories -- only files are backed up.

### 8.5 Preventing Upload of Backup Files

The reference documentation suggests using the `skip_file` configuration to prevent backup files from being uploaded to OneDrive:

```
skip_file = "~*|.~*|*.tmp|*.swp|*.partial|*-safeBackup-*"
```

Without this configuration, backup files will be uploaded to OneDrive as new files during the next sync cycle. This is by design -- both copies (the downloaded remote version and the locally-modified renamed version) end up on OneDrive.

### 8.6 Dry-Run Behavior

When running in `--dry-run` mode, the `safeBackup` function logs what it would do but does not actually perform the rename. The `renamedPath` output parameter remains `null`.
