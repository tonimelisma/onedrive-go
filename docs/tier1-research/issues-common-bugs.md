# Common Bugs & Production Issues (from abraunegg/onedrive issues)

This document catalogs the most impactful bugs reported in the abraunegg/onedrive
Linux client repository (sorted by community engagement / comment count). Each
entry captures what went wrong, the root cause when known, the resolution, and
the lesson our implementation should internalize. The final section distills
these into concrete defensive design patterns.

**Methodology:** The top ~200 most-commented bugs (label: Bug, sorted by comment
count) were reviewed. The top 40+ were investigated in depth. Issues span from
2017 to 2025 and cover Personal, Business, and SharePoint account types.

---

## 1. Sync Logic Bugs

### 1.1 Failed Download Leads to Remote Deletion (#3344, #3015, #3385)

- **What goes wrong:** A file download fails (401 Unauthorized, network error,
  etc.). During the subsequent "database consistency check" phase, the client
  notices the file is missing locally. It concludes the file was deleted locally
  and propagates the deletion to OneDrive, destroying the only good copy.
- **Root cause:** The sync engine does not distinguish between "file was never
  successfully downloaded" and "file was deliberately deleted by the user." The
  delta-then-scan two-phase approach treats any file present in the database but
  absent on disk as a local deletion.
- **Resolution:** Fixed in later versions by tracking download failures and
  excluding failed items from the local-deletion scan.
- **Lesson:** Never delete a remote file based solely on local absence. Maintain
  a separate "pending download" or "download failed" state. Only treat a file as
  locally deleted if it was previously confirmed to exist locally and has since
  been removed.

### 1.2 Upload/Download Infinite Loop for "Enriched" Files (#3070)

- **What goes wrong:** When a PowerPoint or Office file is uploaded to OneDrive
  Business, SharePoint's "enrichment" feature modifies the file (adds metadata,
  changes hashes). The client detects the remote change, downloads the enriched
  version, detects it differs from what it uploaded, re-uploads, and the cycle
  repeats indefinitely.
- **Root cause:** The client does not account for server-side file modification
  ("enrichment") that legitimately changes file content and hashes after upload.
- **Resolution:** Fixed by detecting enrichment (comparing the post-upload server
  hash with the downloaded hash) and accepting the enriched version as
  authoritative after the first round-trip.
- **Lesson:** After uploading a file, accept the server's response metadata (hash,
  size, timestamps) as the new ground truth. Do not re-compare against the
  original local file. If the server modifies content post-upload, download once
  and settle.

### 1.3 Remote State Overwritten by Stale Local State (#3501, #2879)

- **What goes wrong:** Remote changes (renames, edits made via mobile or web) are
  overridden by the Linux client, which mirrors the older local state back to
  OneDrive. For example, a directory renamed via the mobile app gets reverted by
  the client re-uploading the old name.
- **Root cause:** The sync engine's conflict resolution incorrectly favors local
  state even when the remote change is newer. The local scan phase runs after
  the delta phase but can override what delta discovered if timestamps are not
  compared carefully.
- **Resolution:** Ongoing fixes to improve timestamp comparison and conflict
  detection ordering.
- **Lesson:** Always compare modification timestamps and use server-reported
  timestamps as the source of truth for conflict resolution. Never assume local
  state is authoritative without checking recency.

### 1.4 File Rename Clobbering Target Not Synced (#249)

- **What goes wrong:** On Windows, an application renames `A.ext` to
  `A.ext.old_backup` (overwriting the existing backup) and saves a new `A.ext`.
  The delta API reports this as a move. The Linux client creates a
  "conflicting copy" and then deletes the original backup from OneDrive.
- **Root cause:** The sync engine does not handle the case where a move
  operation's target path is already occupied by a different file. It renames the
  conflicting file but then the scan phase sees the original as "deleted locally"
  and removes it remotely.
- **Resolution:** Fixed by improving move-with-overwrite handling.
- **Lesson:** Move operations must handle target-occupied scenarios explicitly:
  check if the destination exists, handle the overwrite semantics, and ensure
  the replaced file is not later double-processed as a deletion.

### 1.5 Short-Lived / Transient Files Cause Errors (#273, #441)

- **What goes wrong:** Applications like vim, LibreOffice, and Calibre create
  temporary files that exist for milliseconds. The inotify watcher picks them up
  and tries to upload them, but by the time the upload starts the file is already
  gone, causing "No such file or directory" errors. In the LibreOffice case, it
  triggers an upload-download-upload loop creating duplicate files.
- **Root cause:** The monitor acts on filesystem events immediately without any
  debounce or settling period. Transient files (swap files, lock files, temp
  saves) are treated identically to permanent files.
- **Resolution:** Added configurable debounce delay (`inotify_delay`) and improved
  skip patterns for temporary files.
- **Lesson:** Implement a debounce/coalescing window for filesystem events (e.g.,
  500ms-2s). Ignore files that are created and deleted within the window. Apply
  skip patterns (`.~*`, `~*`, `*.tmp`, `*.swp`) by default.

### 1.6 Half-Downloaded File Uploaded as New Version (#791)

- **What goes wrong:** Internet dies during download, leaving a truncated file.
  On next sync, the client sees the truncated file as a local modification and
  uploads it to OneDrive, destroying the good remote copy.
- **Root cause:** Downloads are written directly to the final path. If interrupted,
  the partial file sits at the target path and looks like a locally modified
  file.
- **Resolution:** Fixed by downloading to a temporary file and atomically renaming
  on completion.
- **Lesson:** Always download to a temporary file (e.g., `.filename.partial`) and
  rename atomically on completion. Never write directly to the target path. If
  the partial file is detected on startup, delete it and re-download.

### 1.7 sync_list Takes Forever / Never Finishes (#896)

- **What goes wrong:** With `sync_list` configured and a large OneDrive (>100GB),
  the client takes 24+ hours processing items, seemingly stuck in a loop of
  "Processing N OneDrive items to ensure consistent local state."
- **Root cause:** The client downloads the entire delta response (all items on
  the drive) and then filters client-side against sync_list. For large drives
  with small sync_list selections, this is extremely wasteful.
- **Resolution:** Improved filtering and selective delta queries.
- **Lesson:** When selective sync is configured, scope delta queries to the
  relevant subtrees where possible. Perform server-side filtering before
  client-side evaluation. Consider caching and incremental processing.

### 1.8 HTTP 504 Gateway Timeout Causes Local File Deletion (#2338)

- **What goes wrong:** In `--download-only --cleanup-local-files` mode, an HTTP
  504 during the delta query results in an incomplete view of remote state.
  The client then assumes files not in the (partial) response have been deleted
  remotely and removes them locally.
- **Root cause:** Transient API errors during delta enumeration lead to an
  incomplete item set. The cleanup logic does not distinguish between "item
  confirmed absent on server" and "we failed to enumerate the server fully."
- **Resolution:** Fixed by not running cleanup when the delta response is
  incomplete or errored.
- **Lesson:** Never perform destructive operations (deletions) based on
  incomplete data. Track whether a full successful enumeration was completed
  before running any "cleanup" or "orphan removal" logic.

### 1.9 inotify Event Ordering Causes Incorrect Deletion (#2586)

- **What goes wrong:** When vim saves a file (move old away, create new, write
  new), the inotify events arrive as `IN_MOVED_FROM`, `IN_CREATE`,
  `IN_CLOSE_WRITE`. The client processes `IN_CLOSE_WRITE` first (uploading the
  new file), then assumes the `IN_MOVED_FROM` item was deleted (moved outside
  watched directory).
- **Root cause:** Events are not coalesced and the processing order does not
  account for atomic-save patterns (move + create as a single logical operation).
- **Resolution:** Added debounce and better event coalescing.
- **Lesson:** Coalesce filesystem events within a time window. Recognize common
  save patterns (move-then-create, create-temp-then-rename) as single logical
  operations rather than processing each event independently.

### 1.10 Shared Folder Contents Deleted Due to skip_dir Pattern (#3475)

- **What goes wrong:** A `skip_dir` entry of `.*` (intended to skip dotfiles) also
  matches shared folder names. The client deletes all contents of shared folders
  because it interprets them as matching the skip pattern.
- **Root cause:** The glob pattern matching for `skip_dir` is overly broad and
  does not account for how shared folder names are represented internally.
- **Resolution:** Fixed by tightening pattern matching semantics.
- **Lesson:** Filter patterns must be tested against all item types including
  shared folders and remote items. Provide clear documentation on pattern
  semantics and test edge cases thoroughly.

---

## 2. Database / State Corruption

### 2.1 Repeated "Database Consistency Issue" Requiring --resync (#3030, #3165)

- **What goes wrong:** After every sync, the client reports "A database
  consistency issue has been caught. A --resync is needed." Running --resync
  clears the error temporarily, but it returns on the next sync cycle.
- **Root cause:** The database tree structure becomes inconsistent when items
  reference parent IDs that do not exist in the database, often triggered by
  API responses with unexpected or missing fields.
- **Resolution:** Improved database validation and self-healing logic.
- **Lesson:** The database must be self-healing: detect and repair inconsistencies
  rather than requiring a full resync. Implement referential integrity checks
  that can repair (not just detect) broken parent-child relationships.

### 2.2 15-Character driveId API Bug (#3072, #3115, #3136)

- **What goes wrong:** The Microsoft Graph API occasionally returns truncated
  (15-character) driveId values for Personal accounts instead of the correct
  16-character values. This causes database assertion failures, foreign key
  violations, and crashes when computing file paths.
- **Root cause:** A confirmed Microsoft API bug where Personal account driveIds
  are inconsistently returned with varying lengths.
- **Resolution:** Fixed by normalizing driveId values (padding or matching by
  prefix) and handling the inconsistency gracefully.
- **Lesson:** Never trust API-returned identifiers to be consistent. Normalize
  identifiers before using them as database keys. Implement defensive parsing
  that handles known API bugs (truncated IDs, inconsistent casing).

### 2.3 FOREIGN KEY Constraint Failed (#3336, #3169, #3122, #3203)

- **What goes wrong:** Various operations trigger SQLite FOREIGN KEY constraint
  violations, crashing the client. This occurs when inserting items whose parent
  is not yet in the database, or when driveId casing is inconsistent between
  API responses.
- **Root cause:** Microsoft Graph API returns driveIds with inconsistent casing
  (e.g., `b!2iD8jx...` vs `B!2ID8JX...`). SQLite's default string comparison is
  case-sensitive, so these are treated as different drives.
- **Resolution:** Fixed by normalizing driveId casing to lowercase before database
  operations.
- **Lesson:** Normalize all identifier casing before storage. Use
  case-insensitive comparison for identifiers that the API may return
  inconsistently. Process delta items in parent-before-child order to avoid FK
  violations.

### 2.4 WAL File Growing Unbounded (#309)

- **What goes wrong:** The SQLite WAL (Write-Ahead Log) file grows to hundreds
  of megabytes, consuming disk space disproportionate to the actual database
  size.
- **Root cause:** SQLite WAL checkpointing was not being performed regularly,
  allowing the WAL to accumulate indefinitely.
- **Resolution:** Added periodic WAL checkpointing and vacuum operations.
- **Lesson:** Configure SQLite WAL checkpointing explicitly. Run periodic VACUUM
  operations. Monitor database file sizes. Use WAL mode but ensure checkpoint
  thresholds are set.

### 2.5 Database Lock Not Released on Crash (#1954)

- **What goes wrong:** After the process is interrupted (kill, power loss), a lock
  file remains, preventing the application from starting. The error message says
  "onedrive application is already running" but no process exists.
- **Root cause:** The application uses a lock file to prevent concurrent instances
  but does not clean up the lock on abnormal exit.
- **Resolution:** Fixed by using PID-based lock validation (check if the PID in
  the lock file is actually running).
- **Lesson:** Use PID-based lock files that can be validated on startup. Check if
  the recorded PID is still alive. Use advisory file locking (flock) rather than
  sentinel files where possible. Always handle stale locks gracefully.

---

## 3. Network / API Errors

### 3.1 Timeout Stuck Loop (#1908, #3410)

- **What goes wrong:** After a transient network outage (router restart, ISP
  issue), the client gets stuck in an infinite loop spewing timeout error
  messages even after connectivity is restored. In one case (#3410), 175,201
  retry attempts were logged, and 300MB of error logs were generated in minutes.
- **Root cause:** The retry logic does not implement proper backoff, does not
  revalidate the connection handle, and does not have a maximum retry limit that
  results in a graceful restart of the sync cycle.
- **Resolution:** Improved retry logic with exponential backoff and connection
  handle recycling.
- **Lesson:** Implement exponential backoff with jitter for retries. Set a maximum
  retry count per operation (e.g., 5 attempts). After max retries, abandon the
  current operation and restart the sync cycle. Recycle HTTP connection handles
  after errors. Rate-limit log output during error storms.

### 3.2 HTTP 429 Throttling Not Honored (#815, #133)

- **What goes wrong:** Microsoft returns HTTP 429 (Too Many Requests) with a
  `Retry-After` header specifying a backoff period (often 120 seconds). The
  client ignores this header and uses a hardcoded 2-second backoff, leading to
  further throttling and eventual failure.
- **Root cause:** The retry handler did not parse or honor the `Retry-After`
  response header.
- **Resolution:** Fixed by parsing and obeying the `Retry-After` header.
- **Lesson:** Always parse and obey `Retry-After` headers on 429 and 503
  responses. Implement proactive rate limiting based on observed throttling
  patterns. Log when throttled but do not retry faster than the server requests.

### 3.3 Big Upload Fails Due to Expired Access Token (#3355)

- **What goes wrong:** Uploads of very large files (200+ GB) take many hours.
  The access token expires during the upload (tokens are valid for ~24 hours),
  causing a 401 Unauthorized error on a fragment upload. The upload session is
  then abandoned and has to restart from the beginning.
- **Root cause:** The upload fragment URL is pre-authenticated (no Bearer token
  needed), but the client was still sending the Bearer token, and the error
  handling on 401 did not trigger a token refresh and retry for fragment uploads.
- **Resolution:** Fixed by refreshing the token proactively before it expires and
  by not sending the Bearer token with pre-authenticated fragment URLs.
- **Lesson:** Upload session fragment URLs are pre-authenticated -- do not send
  the Bearer token with them. Proactively refresh tokens before they expire
  (e.g., at 80% of lifetime). On 401 during uploads, refresh the token and retry
  the failed fragment rather than restarting the entire upload.

### 3.4 curl SIGPIPE Causes Application Exit (#2874)

- **What goes wrong:** When using curl with unstable connections, excessive
  duplicate network packets cause curl to report "Too old connection." The
  resulting SIGPIPE signal kills the entire application.
- **Root cause:** The application does not ignore SIGPIPE. In D/curl, a broken
  pipe during HTTP communication raises SIGPIPE, which has a default action of
  process termination.
- **Resolution:** Fixed by ignoring SIGPIPE at the application level.
- **Lesson:** Ignore SIGPIPE at application startup (Go does this by default).
  Handle all network errors gracefully at the application level rather than
  allowing OS signals to terminate the process.

### 3.5 Corrupt Files Downloaded Silently (#558)

- **What goes wrong:** OneDrive API returns error responses (JSON error objects,
  HTML error pages, 503 "Service Unavailable") instead of actual file content.
  The client writes this error content to disk as if it were the file, replacing
  the real file with garbage.
- **Root cause:** The download code does not validate the response status code or
  content type before writing to disk. It also does not verify the downloaded
  file's hash or size against the expected values.
- **Resolution:** Fixed by validating response status codes and performing
  post-download hash/size verification.
- **Lesson:** Validate HTTP response status codes before processing the body.
  Always verify downloaded file hash and size against the server-reported values.
  Download to a temporary file and only promote to the final path after
  validation passes. If validation fails, delete the temporary file and retry.

### 3.6 Invalid ISO Timestamp Crashes Application (#2813, #2810)

- **What goes wrong:** The Microsoft API returns malformed or garbage timestamp
  strings (e.g., garbled Unicode instead of ISO 8601). The D datetime parser
  throws a `TimeException` and crashes the application.
- **Root cause:** The API occasionally returns corrupt metadata, and the client
  does not validate timestamps before parsing.
- **Resolution:** Fixed by adding defensive timestamp parsing with fallback
  values.
- **Lesson:** Validate all API response fields before parsing. Use defensive
  parsing with fallback values for timestamps, sizes, and other metadata. Never
  let a parsing error crash the application; log the anomaly and skip or use a
  safe default.

---

## 4. File System Edge Cases

### 4.1 Special Characters in Filenames (#35, #2740, #2823)

- **What goes wrong:** Files with non-printable characters, HTML-encoded entities
  (e.g., `%20`, `&amp;`), or special Unicode characters cause crashes, filter
  mismatches, or incorrect path construction. In #2740, users had to add filter
  entries twice (once with HTML encoding, once without) to match files.
- **Root cause:** Inconsistent handling of URL encoding in path comparisons and
  filter matching. The API returns paths with URL-encoded characters, but local
  paths use raw characters.
- **Resolution:** Fixed by normalizing path encoding before comparison.
- **Lesson:** Normalize all paths to a canonical form (decoded Unicode) before
  storage, comparison, or filter matching. Handle URL encoding/decoding at the
  API boundary only. Test with Unicode, spaces, quotes, percent signs, and
  non-printable characters.

### 4.2 Invalid UTF-8 Sequences (#3014)

- **What goes wrong:** Files or directories with invalid UTF-8 byte sequences in
  their names cause the D regex engine to crash with `UTFException`.
- **Root cause:** The filter matching code uses regex on potentially invalid
  UTF-8 strings from the local filesystem without validation.
- **Resolution:** Fixed by validating UTF-8 sequences before regex processing.
- **Lesson:** Validate all strings from the local filesystem for UTF-8 validity
  before processing. In Go, use `utf8.Valid()` and replace or skip invalid
  sequences. The local filesystem may contain filenames that are not valid UTF-8.

### 4.3 Path Length Limits (#134, #142)

- **What goes wrong:** Files with long Unicode paths are incorrectly flagged as
  exceeding OneDrive's 430-character path limit because the client URL-encodes
  the path before measuring length. Cyrillic/CJK characters that are 1 character
  expand to 6+ characters when URL-encoded. This causes false "path too long"
  warnings (though the files do sync).
- **Root cause:** OneDrive measures path length in characters (not URL-encoded
  bytes), but the client was measuring encoded length for all account types.
  Business accounts use encoded length; Personal accounts use character length.
- **Resolution:** Fixed by using different length calculations for Business vs
  Personal accounts.
- **Lesson:** Measure path length in characters for OneDrive Personal (400 char
  limit) and in URL-encoded bytes for OneDrive Business (400 byte limit).
  Validate path lengths before attempting operations and give clear error
  messages. Also validate against local filesystem limits (typically 255 bytes
  per component on Linux).

### 4.4 Case-Insensitive Filename Collisions (#3237)

- **What goes wrong:** Two different files with names differing only in case
  (e.g., `Report.pdf` and `report.pdf`) trigger a "case-insensitive match"
  error. On case-insensitive filesystems (macOS default, some Windows mounts),
  these would overwrite each other.
- **Root cause:** OneDrive is case-insensitive but case-preserving. Linux
  filesystems are typically case-sensitive. The client must reconcile these
  different semantics.
- **Resolution:** Fixed by detecting case collisions and handling them.
- **Lesson:** Implement case-insensitive collision detection regardless of the
  local filesystem. When two items would map to the same local path on a
  case-insensitive filesystem, rename one. Store the OneDrive canonical (case-
  preserved) name in the database and compare case-insensitively.

### 4.5 iOS .heic File Hash/Size Mismatch (#2471)

- **What goes wrong:** Files uploaded from iOS (particularly .heic images) report
  different sizes and hashes when downloaded compared to what the API metadata
  says. Download validation fails, but the files are actually correct.
- **Root cause:** A confirmed Microsoft API bug where the server transcodes or
  modifies .heic files between upload and download, but the metadata retains the
  original values.
- **Resolution:** Documented as an API bug. Users work around it with
  `--disable-download-validation`.
- **Lesson:** Provide a per-file-type or per-extension option to skip validation
  when known API bugs cause mismatches. Log validation failures as warnings
  rather than errors when the file type is known to be problematic. Track known
  API bugs in a compatibility table.

### 4.6 Timestamp Discrepancies Cause Unnecessary Syncs (#3057, #2591)

- **What goes wrong:** Local and remote timestamps differ by small amounts
  (seconds), causing the client to detect changes and re-upload or re-download
  files unnecessarily. In download-only mode, the client incorrectly updates
  remote timestamps.
- **Root cause:** Filesystem timestamp resolution varies (ext4: 1ns, FAT32: 2s,
  NTFS: 100ns). The API may round timestamps differently. Time zone conversions
  can also introduce discrepancies.
- **Resolution:** Fixed by using hash comparison as the primary change detector
  when timestamps differ by small amounts, and by not modifying remote timestamps
  in download-only mode.
- **Lesson:** Use content hash as the definitive change detector, not timestamps
  alone. Allow a configurable timestamp tolerance (e.g., 2 seconds) to account
  for filesystem resolution differences. In read-only modes, never modify the
  remote.

---

## 5. Shared Folder Issues

### 5.1 Shared Folder Shortcuts Synced to Wrong Location (#3407, #3643, #3302)

- **What goes wrong:** Shared folder shortcuts that have been renamed and/or moved
  into subfolders are always synced to the root of the sync directory under their
  original names. Empty placeholder directories are also created at the correct
  (renamed/relocated) path.
- **Root cause:** The client does not properly follow the shortcut's current
  parent reference and display name, instead using the shared folder's original
  identity.
- **Resolution:** Partially fixed in v2.5.6 via #3303 and #3308, but regressed
  in v2.5.10 (#3643).
- **Lesson:** Shared folder shortcuts are references that have their own metadata
  (name, parent) independent of the target folder. Always use the shortcut's
  metadata for local placement, not the target's. Test extensively with renamed
  and relocated shortcuts.

### 5.2 Shared Folder Contents Unexpectedly Deleted (#3475, #3158)

- **What goes wrong:** The client deletes all contents of shared folders during
  sync. In #3475, a `skip_dir` pattern of `.*` inadvertently matched shared
  folder internal representations. In #3158, OneNote files in shared business
  folders were deleted.
- **Root cause:** Shared folders have different internal representations
  (different drive IDs, remote item references) that interact unexpectedly with
  filtering and deletion logic.
- **Resolution:** Fixed by improving shared folder handling in filter evaluation.
- **Lesson:** Shared folders require separate, careful handling throughout the
  sync pipeline. Apply an extra safety check before deleting anything in a
  shared folder (e.g., require explicit confirmation or use a higher deletion
  threshold). Test all filter patterns against shared folder scenarios.

### 5.3 Cross-Drive Inconsistent API Responses (#3136, #966)

- **What goes wrong:** Shared folders from other users' drives return inconsistent
  API responses: different driveId formats, missing fields, different casing. The
  database cannot maintain referential integrity across drives.
- **Root cause:** Microsoft Graph API returns different response shapes depending
  on the drive type (Personal, Business, SharePoint) and whether the item is
  accessed via the owner's drive or the shared view.
- **Resolution:** Fixed by normalizing cross-drive references and handling
  missing fields defensively.
- **Lesson:** Design the database schema to handle multiple drives with different
  ID formats from the start. Normalize all IDs to a canonical form. Expect and
  handle missing fields in API responses for shared/remote items.

---

## 6. Performance Issues

### 6.1 100% CPU When Idle (#404, #394, #21, #693)

- **What goes wrong:** The client consumes 100% of a CPU core even when there are
  no changes to sync. This persists across versions and affects both monitor and
  sync modes. In #693, CPU spikes occur after resuming from sleep with no
  network.
- **Root cause:** Multiple causes identified: (a) busy-loop polling in the
  monitor when inotify returns no events, (b) continuous directory traversal for
  "validation" that re-scans every file, (c) inefficient string processing
  in the D runtime during path computation.
- **Resolution:** Improved across many versions with better inotify handling,
  reduced redundant scanning, and optimized path computation. With 45,000+
  files, the validation phase alone caused persistent high CPU.
- **Lesson:** Use blocking I/O (epoll/kqueue) for filesystem monitoring, never
  busy-loop polling. Minimize full directory scans -- use incremental change
  detection. Profile with realistic file counts (10K-100K files). Set a CPU
  budget for idle monitoring.

### 6.2 SQLite Performance at Scale (#309, #468)

- **What goes wrong:** With large sync directories (100K+ files), database
  operations become slow, the WAL file grows unbounded, and the application
  becomes unresponsive.
- **Root cause:** Missing indexes, unoptimized queries, lack of WAL
  checkpointing, and holding transactions open too long.
- **Resolution:** Added database vacuum, checkpoint operations, and query
  optimization.
- **Lesson:** Design the database schema with indexes for all common query
  patterns (lookup by ID, by parent, by path). Use prepared statements.
  Implement WAL mode with periodic checkpointing. Batch database writes in
  transactions. Test with 100K+ items.

---

## 7. Platform-Specific Bugs

### 7.1 curl Version Incompatibilities (#314, #220, #224, #2874, #3320)

- **What goes wrong:** Various curl versions have bugs that affect the client.
  curl 7.62.0-7.63.0 broke HTTP/2 downloads. curl 7.81.0 had connection
  recycling issues. curl 8.5.0 has HTTP/2 bugs that the client now detects
  and works around.
- **Root cause:** The D client links directly against the system's libcurl,
  inheriting whatever bugs are present in that version.
- **Resolution:** Added `--force-http-1.1` flag, curl version detection with
  automatic fallback, and connection management improvements.
- **Lesson:** In Go, use the standard `net/http` library rather than linking
  against curl. This eliminates an entire class of platform-specific
  dependency bugs. If HTTP/2 issues are encountered, implement a fallback to
  HTTP/1.1. Test against multiple HTTP library versions.

### 7.2 Docker / Container Issues (#2951, #520, #306, #3208)

- **What goes wrong:** Various container-specific issues: config files not found
  when running as different UID/GID, Alpine container SSL issues, user not
  listed in `/etc/passwd` causing crashes.
- **Root cause:** The application makes assumptions about the runtime environment
  (home directory exists, user has a passwd entry, SSL certificates are at
  standard paths) that do not hold in containers.
- **Resolution:** Fixed by removing environment assumptions and adding
  configuration overrides.
- **Lesson:** Do not assume environment: home directory, passwd entries, SSL
  certificate paths, or filesystem permissions. Allow all paths to be
  configurable. Test in minimal container environments (Alpine, scratch). Handle
  missing environment variables gracefully.

### 7.3 NFS / Network Filesystem Issues (#3030, #1137, #1365)

- **What goes wrong:** When the sync directory is on NFS or other network
  filesystems, various issues arise: database corruption, permission errors
  ("Read-only file system"), timestamp inconsistencies, and inotify not working
  (inotify does not work on NFS).
- **Root cause:** The application assumes a local POSIX filesystem with
  reliable inotify support, consistent timestamps, and atomic rename operations.
- **Resolution:** Various fixes including better error handling for filesystem
  operations.
- **Lesson:** Keep the database on a local filesystem, even if the sync directory
  is on a network mount. Detect network filesystems and warn about inotify
  limitations. Use polling as a fallback when inotify is unavailable. Do not
  assume atomic rename works across filesystem boundaries.

### 7.4 Retention Policy Prevents Deletion (#338)

- **What goes wrong:** OneDrive Business with retention policies enabled returns
  HTTP 403 when trying to delete non-empty directories. The client crashes or
  retries indefinitely.
- **Root cause:** Business retention policies prevent deletion of items on hold.
  The client attempts top-down directory deletion instead of bottom-up, hitting
  the "cannot delete non-empty folder" restriction.
- **Resolution:** Fixed by implementing bottom-up (leaf-first) deletion and
  handling 403 errors on deletion gracefully.
- **Lesson:** Delete directories bottom-up (files first, then empty directories).
  Handle 403 on deletion gracefully (skip, log, report to user). Support
  OneDrive Business retention policies by not assuming all items are deletable.

---

## Defensive Design Patterns

Based on the patterns across 200+ bugs, our implementation should adopt these
concrete design choices:

### Data Safety

1. **Download to temporary, rename on completion.** Never write directly to the
   target path. Use `.filename.tmp` or a staging directory. Verify hash and size
   before promoting. This prevents corruption from interrupted downloads (#791,
   #558).

2. **Never delete remote files based on local absence alone.** Maintain a "last
   known good" state for each file. Only treat a file as locally deleted if it
   was previously confirmed present locally after a successful sync. Track
   download failures separately (#3344, #3015, #3385).

3. **Never perform destructive operations on incomplete data.** If a delta query
   or enumeration fails partway through, do not run cleanup or deletion logic.
   Require a complete successful enumeration before computing deletions (#2338).

4. **Classify all big deletes.** If a sync cycle would delete more than N items
   (configurable, default 100), pause and require user confirmation. This catches
   pattern matching errors, API failures, and edge cases before they destroy data
   (#3475, #3475).

### API Resilience

5. **Normalize all identifiers.** Lowercase driveIds, normalize casing, handle
   truncated IDs, and pad/match by prefix. Never use raw API-returned identifiers
   as database keys without normalization (#3072, #3115, #3336).

6. **Defensive JSON parsing.** Never crash on missing fields, unexpected types,
   or malformed values. Every field access should have a default/fallback. Log
   anomalies but continue processing (#2813, #540, #167).

7. **Respect Retry-After headers.** Parse and obey `Retry-After` on 429 and 503
   responses. Implement exponential backoff with jitter for transient errors.
   Cap retries per operation at ~5 attempts, then skip and continue (#815, #133).

8. **Refresh tokens proactively.** Refresh OAuth tokens at 80% of their lifetime,
   not on expiry. Do not send Bearer tokens with pre-authenticated upload
   fragment URLs (#3355).

9. **Handle server-side file modification.** After uploading, accept the server
   response metadata as ground truth. If the server modifies the file
   (enrichment, transcoding), download once and settle. Do not enter an
   upload-download loop (#3070).

### Database Design

10. **Self-healing database.** Detect and repair referential integrity issues
    automatically rather than requiring `--resync`. Use `ON DELETE CASCADE` and
    handle orphaned records gracefully (#3030, #3165).

11. **WAL mode with checkpointing.** Use SQLite WAL mode for performance but
    checkpoint regularly (every N transactions or M seconds). Run periodic VACUUM.
    Monitor WAL file size (#309).

12. **Process items in dependency order.** When applying delta responses, sort
    items so parents are inserted before children. This prevents FK violations
    (#3336, #3169, #3122).

### Filesystem Handling

13. **Debounce filesystem events.** Coalesce events within a 1-2 second window.
    Recognize atomic-save patterns (move+create, create-temp+rename). Ignore
    files that are created and deleted within the window (#273, #441, #2586).

14. **Validate UTF-8 and special characters.** Validate all filesystem-sourced
    strings for UTF-8 validity. Normalize Unicode (NFC). Handle URL encoding at
    API boundaries only. Test with every category of special character (#35,
    #3014, #2740).

15. **Use content hash as primary change detector.** Timestamps are unreliable
    across filesystems, time zones, and API roundtrips. Use QuickXorHash as the
    definitive "has this file changed?" signal, with timestamp as a fast-path
    optimization (#3057, #2591).

### Network and Connection Management

16. **Use Go's net/http, not libcurl.** This eliminates an entire category of
    platform-specific bugs that plagued the D implementation (#314, #220, #2874,
    #3320).

17. **Bounded retry with circuit breaker.** After N consecutive failures to the
    same endpoint, enter a backoff state. Do not spin in an infinite retry loop.
    Limit error log output to prevent log flooding (#1908, #3410).

18. **Ignore SIGPIPE.** Go does this by default, but ensure no subprocess or CGo
    code reintroduces SIGPIPE sensitivity (#2874).

### Shared Folder Safety

19. **Treat shared folders as a separate concern.** They have different drive
    IDs, different API response shapes, and different deletion semantics. Apply
    extra safety checks before modifying shared content. Use the shortcut's
    metadata for local placement, not the target's (#3407, #3643, #3475).

20. **Cross-drive ID normalization.** Store a canonical drive ID mapping. Handle
    the fact that the same logical drive may appear with different ID formats
    in different API contexts (#3136, #3072).

### Operational

21. **PID-based locking.** Use advisory file locks (`flock` on Linux, `fcntl`
    locks) rather than sentinel files. Validate locks on startup by checking
    if the recorded PID is alive (#1954).

22. **Structured logging with level control.** Rate-limit repeated error messages.
    Include operation context (item ID, path, HTTP status) in every log entry.
    Support debug/verbose modes without changing code (#3410 generated 300MB of
    logs in minutes).

23. **Idempotent sync operations.** Every sync cycle should be safe to interrupt
    and restart. No operation should leave the system in a state where the next
    sync cycle produces different results than if the operation had never started.
