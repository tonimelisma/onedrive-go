# Edge Cases, Gotchas, and Hard-Won Lessons from the Reference Implementation

This document catalogs known edge cases, API quirks, workarounds, and pitfalls discovered through the reference implementation's development and issue tracker. It is intended as a field guide for building a clean-room OneDrive sync client.

---

## 1. OneDrive API Edge Cases

### 1.1 Missing Fields in API Responses

**Name field omitted on deleted items (Business).** When a DriveItem is deleted on OneDrive for Business, the delta response omits the `name` property entirely. The sync engine cannot rely on `name` being present when handling deleted items in Business accounts. It must look up the name from its local database instead.

**cTag omitted in multiple contexts.** The content tag (`cTag`) is not returned in several important situations:
- For folders on OneDrive for Business (never present)
- For the root item on OneDrive for Business
- In delta query create/modify responses on OneDrive for Business
- For deleted items on both Personal and Business accounts

This means a sync client cannot rely on `cTag` for change detection in Business accounts and must use other signals (eTag, file hashes, timestamps).

**fileSystemInfo missing on remote items.** When a shared folder is added via "Add shortcut to My files" in the OneDrive web UI, the resulting remote item in the user's drive may not include `fileSystemInfo` in the `remoteItem` facet. The sync engine must fall back to the top-level `fileSystemInfo` or to the current time when this occurs.

**eTag not returned for root item (Business).** The root item of OneDrive for Business drives does not return an `eTag`. The sync engine should not depend on eTag for concurrency control on root items.

**lastModifiedDateTime missing on API-deleted items.** When OneDrive itself deletes an item (as opposed to a user-initiated deletion), the `lastModifiedDateTime` field may not be returned. This is documented in the OneDrive API docs (issue #834). The sync engine should default to the current time when timestamps are absent.

**size omitted on Personal deleted items.** When an item is deleted on OneDrive Personal, the `size` property is omitted from the delta response, along with `cTag`.

### 1.2 Invalid Timestamps from the API

The OneDrive API can return invalid or nonsensical timestamps. The reference implementation validates all incoming timestamp strings and falls back to the current UTC time if they fail validation. Timestamps should be validated before being stored in the local database or applied to the filesystem.

Additionally, OneDrive does not store fractional seconds. When comparing timestamps between local files and server-reported values, fractional seconds must be truncated to zero before comparison. Failing to do this causes false positives for change detection.

### 1.3 Inconsistent Behavior Between Personal and Business

**Hash availability differs.** OneDrive Personal provides SHA1 and CRC32 hashes. OneDrive for Business provides SHA256. QuickXorHash is the only hash available on both. A sync client must implement QuickXorHash as its primary content verification mechanism.

**driveId format inconsistency (Personal API Bug).** On OneDrive Personal, the API sometimes returns `parentReference.driveId` values that are shorter than the expected 16 characters. The reference implementation detects this condition (tracked as Issue #3072) and either corrects the value by matching against the known default drive ID or pads with leading zeros. This is a confirmed API bug specific to Personal accounts.

**Shared items JSON structure.** On Personal accounts, shared items occasionally lack the `shared` JSON structure entirely, even though they should have it. The reference implementation logs this as a potential API bug and handles it gracefully.

**Permanent deletion not supported on all account types.** The `DELETE /items/{id}` endpoint with permanent deletion is only supported on OneDrive for Business and SharePoint document libraries. OneDrive Personal accounts do not support permanent deletion via the API -- items always go to the recycle bin. Additionally, National Cloud Deployments may not support permanent deletion regardless of account type.

### 1.4 Delta Query Quirks

**Items appear multiple times.** A single delta response can include the same item multiple times. The client must always use the last occurrence, as it represents the most current state.

**parentReference.path is never included.** Delta responses never include the `path` property within `parentReference`. The sync engine must maintain its own path-to-ID mapping by tracking `parentReference.id` and building paths from parent-child relationships.

**Renaming a parent folder does not produce delta entries for descendants.** If a folder is renamed, only that folder appears in the delta response. None of its children or grandchildren get delta entries. The sync engine must infer path changes for all descendants by walking the parent-child relationship graph.

**deltaLink in the last bundle creates a retry gap.** The `@odata.deltaLink` that finalizes a checkpoint only appears in the last response bundle. If any file downloads fail within that final bundle and the deltaLink is saved, the only way to retry those failed items is a full resync. This is an API capability gap -- there is no mechanism to "partially commit" a delta checkpoint.

**Delta token invalidation (HTTP 410 Gone).** Delta tokens can expire or become invalid. The server responds with HTTP 410 and one of two resync directives:
- `resyncChangesApplyDifferences` -- replace local with server state, upload local unknowns
- `resyncChangesUploadDifferences` -- upload local items the server did not return

**National Cloud Deployments do not support delta queries.** The US Government, Germany, and China cloud deployments do not support the `/delta` endpoint. The reference implementation falls back to `/children` enumeration for these environments, which is significantly less efficient. A configuration flag (`force_children_scan`) can also force this behavior for testing.

**Timestamp-based tokens (Business only).** OneDrive for Business supports passing a URL-encoded timestamp as a delta token (e.g., `?token=2021-09-29T20%3A00%3A00Z`) to get changes since a specific time. This is not available on Personal accounts.

**latest token returns empty results.** Calling delta with `?token=latest` returns zero items and a deltaLink. This is useful to begin tracking from "now" without full enumeration, but the client will miss all pre-existing state.

### 1.5 Race Conditions with Rapid Changes

When a file is modified on the server while a delta response is being paginated, the file can appear in an inconsistent state across pages. The "use last occurrence" rule partially mitigates this, but the client should be prepared for an item's properties to change between the time it appears in delta and the time it is actually downloaded.

### 1.6 Items That Appear and Disappear

**Microsoft OneNote items.** OneNote notebooks appear in the drive hierarchy as "package" items (with a `package` facet of type `oneNote`). They should not be synced as regular files or folders. The reference implementation identifies and skips these by:
- Checking for the `package` facet with `type: "oneNote"`
- Checking for `application/msonenote` or `application/octet-stream` MIME types with `.one` or `.onetoc2` file extensions
- Checking for the special `OneNote_RecycleBin` folder name

**Zero-hash deleted items.** When items are marked as deleted in the API, they sometimes include a bogus hash value (e.g., `quickXorHash: "AAAAAAAAAAAAAAAAAAAAAAAAAAA="`). This makes hash-based comparison useless for confirming whether a local file matches the (now-deleted) online version. The reference implementation falls back to comparing the local file hash against the previously-stored database hash instead.

---

## 2. Filesystem Edge Cases

### 2.1 Symlinks

Symlinks require careful handling and the reference implementation provides a `skip_symlinks` configuration option to skip them entirely. When symlinks are not skipped:

- **Broken symlinks** (where the target does not exist) are detected and skipped with a warning.
- **Relative symlinks** require special handling. The reference implementation changes directory to the symlink's parent, resolves the relative target, then restores the original working directory. This is necessary because `readLink()` on a relative symlink returns a relative path that may not resolve from the sync directory root.
- **Symlinks as sync_dir targets** are supported. When the sync directory itself is a symlink to a mounted volume, the `.nosync` guard file mechanism (see section 2.10) still works correctly because the file becomes visible when the mount disappears.
- **Safety during removal.** The reference implementation explicitly checks for symlinks when traversing parent directories during cleanup removal, refusing to descend into or remove symlinked parent directories.

### 2.2 Hard Links

Hard links are not explicitly handled by the reference implementation. Since OneDrive is a cloud storage service with no concept of hard links, each hard link to the same inode would appear as an independent file. There is no special detection or deduplication.

### 2.3 Special Characters in Filenames

OneDrive and SharePoint enforce strict naming rules that differ from POSIX filesystems. The reference implementation validates all names against these rules before attempting upload:

**Disallowed names (case-insensitive):** `.lock`, `desktop.ini`, `CON`, `PRN`, `AUX`, `NUL`, `COM0`-`COM9`, `LPT0`-`LPT9`

**Disallowed patterns:**
- Names starting with `~$` (Office temporary files)
- Names containing `_vti_` (SharePoint internal directories)
- Names named `forms` at the root level (SharePoint reserved)

**Invalid characters:** `"`, `*`, `:`, `<`, `>`, `?`, `/`, `\`, `|`

**Invalid formatting:**
- Leading whitespace
- Trailing whitespace
- Trailing dot (`.`) -- this is not documented in the official Microsoft documentation but is enforced in practice (reference issue #2678)

### 2.4 Case Sensitivity / Insensitivity Conflicts

This is one of the most significant cross-platform issues. OneDrive uses a case-insensitive namespace (consistent with Windows/NTFS), but Linux filesystems are typically case-sensitive. This means:

- Two files named `README.md` and `Readme.md` can coexist on Linux but not on OneDrive.
- The reference implementation detects POSIX "case-insensitive matches" -- situations where a local name differs from an online name only in case. These are flagged as `PosixException` violations.
- The reference implementation maintains a list of `posixViolationPaths` that cannot be created online due to case conflicts.
- When traversing paths for `--single-directory` sync, every path segment in the entire hierarchy could be a case-insensitive match, requiring careful validation at each level.

### 2.5 Very Long File Paths

The reference implementation does not enforce a specific path length limit, but OneDrive has platform-dependent constraints. Windows limits full paths to approximately 400 characters (including the OneDrive folder path). SharePoint limits individual path segments and total URL length. Files that exceed these limits can exist on Linux but will fail to upload.

### 2.6 Very Large Files

The maximum file size is 250 GB for all account types (increased from 100 GB in January 2021). Files up to 4 MiB can use simple upload (PUT). Files larger than 4 MiB must use upload sessions (resumable upload). The default upload fragment size is 10 MiB, with a maximum of 60 MiB per fragment.

### 2.7 Zero-Byte Files

Zero-byte files are a valid edge case. The reference implementation does not special-case them for upload, but they interact with hash comparison: a zero-byte file has a known, fixed hash. Beware that Windows OneDrive's "Files On-Demand" feature represents cloud-only files as 0-byte placeholder links. If a sync directory is shared between Windows and Linux in a dual-boot scenario, all files may appear as 0-byte stubs (see section 7.1).

### 2.8 File Permission Issues

The reference implementation provides configurable permission modes:
- `sync_dir_permissions` -- permissions for newly created directories (default: platform default)
- `sync_file_permissions` -- permissions for newly created files (default: platform default)

The refresh token file is stored with restricted permissions (owner-only read/write) to prevent credential leakage.

The `setTimes()` operation (setting file modification timestamps) can fail if the file disappears between the time it is downloaded and the time the timestamp is applied. The reference implementation uses a retry wrapper (`safeSetTimes`) that attempts up to 5 retries, handling `ENOENT` gracefully when the file vanishes.

### 2.9 Files That Change During Sync

Files can be modified by other processes while a sync operation is in progress. This is particularly problematic with:
- **File indexers** (Baloo File Indexer, Tracker3) that access files during sync, potentially triggering spurious modification timestamps.
- **Office suites** that use "delete-and-replace" save semantics (notably WPS Office), which can cause the sync engine to see a deletion followed by a creation rather than a modification.
- The reference implementation has documented data loss incidents on SharePoint libraries when file indexers or WPS Office interact with the sync directory.

### 2.10 Mounted Directories and the .nosync Guard

When the sync directory is a mount point (NFS, CIFS, USB), the mount can disappear (network loss, device removal). The client has no knowledge of mount events and will interpret a missing mount as "all files deleted," potentially deleting everything online.

Safeguards include:
- `classify_as_big_delete` -- if more than N items (default 1000) would be deleted, the operation is aborted
- `check_nomount` / `check_nosync` -- the client checks for a `.nosync` file in the sync directory root. This file is placed on the underlying filesystem before mounting. When the mount is active, the file is hidden. If the mount disappears, the file becomes visible, and the client halts.

### 2.11 Extended Attributes (xattr)

The reference implementation includes an `xattr` module for reading and writing extended file attributes. This is implemented for Linux (via `setxattr`/`getxattr`) and FreeBSD (via `extattr_set_file`/`extattr_get_file`). On unsupported platforms (e.g., macOS), xattr operations throw an exception. Extended attributes may be used to store sync metadata directly on files.

---

## 3. OneDrive for Business Specifics

### 3.1 SharePoint Library Quirks

**Upload validation must be disabled.** When uploading files to SharePoint document libraries, SharePoint may modify the file content post-upload by embedding library metadata into the file itself. This causes the hash of the uploaded file to differ from the hash of the original local file, making post-upload integrity validation fail. The reference implementation provides a `disable_upload_validation` flag for this case. This is documented in the OneDrive API docs issue #935.

**Download validation may also fail.** Similarly, when downloading files from SharePoint, the API may not report the correct file size, causing size-based download validation to fail. A `disable_download_validation` flag exists for this.

**Quota information is restricted.** SharePoint drives frequently restrict or omit quota information. The API may return empty, blank, negative, or zero values for quota details. The sync engine must handle this gracefully.

**Each SharePoint library is a separate drive.** A SharePoint document library has its own drive ID, requiring a separate delta query. The reference implementation requires separate configuration directories and sync directories for each library.

**drive_id changes require resync.** Changing the `drive_id` configuration (e.g., switching to a different SharePoint library) requires a full resynchronization (`--resync`). The database state is invalidated by such a change.

### 3.2 Shared Folder Behavior

**Shared folders use remoteItem facet.** Shared folders from other users appear in the user's drive with a `remoteItem` facet pointing to the actual item on the source drive. Operations on shared content must address items using `remoteDriveId` and `remoteId`, not the local item's own ID.

**Each shared folder requires its own delta query.** Because shared folders live on a different drive, they need a separate `/delta` call using the remote drive's ID. Querying the user's own drive with delta will not return changes within shared folders.

**External shared folders are invisible to the API.** Folders shared with you from people outside your organization are not returned by the Microsoft Graph API at all. These appear in the OneDrive web UI with a "world" icon but cannot be synced programmatically (tracked in reference issue #966).

**Shared files with read-only (no download) permissions.** Some shared files have permissions that allow viewing but not downloading. Attempting to download these returns HTTP 403. The sync engine must handle this gracefully rather than treating it as a fatal error.

### 3.3 "Add shortcut to My files" Issues

The "Add shortcut to My files" feature creates a remote item reference in the user's drive. These shortcuts:
- Require `sync_business_shared_items = "true"` in the configuration
- Trigger a `--resync` requirement when first enabled
- May lack `fileSystemInfo` in the `remoteItem` facet
- Have different names locally vs. on the remote drive (the `actualOnlineName` may differ from the display name)

### 3.4 Different Namespace Handling

Business accounts use a different driveId format and item ID format compared to Personal accounts. The driveId is typically longer and more complex (e.g., `b!6H_y8B...xU5`). The sync engine should treat IDs as opaque strings and never assume a particular format.

---

## 4. OneDrive Personal Specifics

### 4.1 Shared Items Limitations

OneDrive Personal handles shared items differently from Business. The reference implementation retrieves remote items from the Personal account's root differently, using the database's remote item records. The `shared` JSON structure may be missing from Personal shared items, which is logged as a potential API bug.

### 4.2 Personal Vault Behavior

Personal Vault (the encrypted, 2FA-protected area in OneDrive Personal) is not explicitly addressed by the reference implementation. These files exist in a special folder that requires additional authentication to access. A sync client should be prepared for API errors when attempting to access vault contents without proper authorization.

### 4.3 Photo / Camera Roll Special Handling

OneDrive has special folders (accessed via `/drive/special/{name}`) for `photos` and `cameraroll`. These are regular folders with a `specialFolder` facet. The reference implementation does not apply any special treatment to these -- they sync like normal folders.

---

## 5. Network & Authentication Edge Cases

### 5.1 Token Expiry During Long Syncs

OAuth2 access tokens expire periodically (typically within 1 hour). The reference implementation pre-emptively checks token expiry before each API call via `checkAccessTokenExpired()`. This is especially critical during long upload sessions where individual fragment uploads could span the token expiry boundary. The token check occurs:
- Before every HTTP request
- Specifically before each upload fragment PUT in a session upload

If the token has expired, a new one is obtained using the refresh token before proceeding with the request.

### 5.2 Network Interruption Recovery

**Upload session resumption.** When an upload session is interrupted (network failure, application crash), the reference implementation saves session state to a file (`session_upload.UNIQUE_STRING`). On the next run, it detects these interrupted session files and resumes the upload from where it left off. Upload session URLs are pre-authenticated and do not require a Bearer token.

**Download resumption.** Similarly, interrupted downloads are tracked via `resume_download.UNIQUE_STRING` files and can be resumed on subsequent runs.

**resync clears interrupted transfers.** Using `--resync` clears all interrupted upload and download records, requiring them to start from scratch.

### 5.3 Connection Failures with European Data Centers

A specific class of connection failures predominantly occurs with Microsoft's European data centers, manifesting as:
```
OpenSSL SSL_read: SSL_ERROR_SYSCALL, errno 104
```
This is caused by unstable connections or HTTPS transparent inspection proxies. The reference implementation recommends forcing HTTP/1.1 (`force_http_11`) and IPv4-only (`ip_protocol_version`) as workarounds. The error code 141 (SIGPIPE) often accompanies these failures.

### 5.4 Rate Limiting Patterns

The OneDrive API returns the following HTTP codes for throttling:

| Code | Meaning | Handling |
|------|---------|----------|
| 429 | Too Many Requests | Read `Retry-After` header, wait that many seconds |
| 503 | Service Unavailable | Retry after 30 seconds |
| 504 | Gateway Timeout | Retry after 30 seconds |
| 509 | Bandwidth Limit Exceeded (SharePoint) | Back off significantly |

The reference implementation uses exponential backoff for transient errors (408, 429, 503, 504), with a maximum backoff of 120 seconds. After exceeding the maximum retry count (approximately 365 days of cumulative retries), it gives up.

For 429 specifically, the `Retry-After` response header value is used directly rather than the calculated exponential backoff.

**User-configurable rate limiting.** The reference implementation allows users to set a client-side `rate_limit` (in bytes/second). If set below 131072 (128 KB/s), it is overridden to the minimum to prevent application timeouts.

### 5.5 Proxy Issues

The reference implementation relies on curl for all HTTP operations. Proxy configuration is inherited from the system's curl/environment configuration. HTTPS transparent inspection proxies are known to cause connection failures (see section 5.3).

---

## 6. Known Bugs & Workarounds

### 6.1 OneDrive Personal driveId Truncation Bug (Issue #3072)

The OneDrive Personal API sometimes returns `parentReference.driveId` values that are shorter than 16 characters. The reference implementation detects this and applies corrections:
- If the truncated value is a substring of the known default drive ID, it uses the full default drive ID.
- Otherwise, it pads the value with leading zeros to reach 16 characters.

### 6.2 SharePoint Post-Upload File Modification (API Issue #935)

When files are uploaded to SharePoint document libraries, SharePoint may inject metadata from the library schema directly into the file content. This silently modifies the file after upload, causing the server-side hash to differ from the client's expected hash. The workaround is to disable upload validation for SharePoint libraries (`disable_upload_validation`).

Reference: https://github.com/OneDrive/onedrive-api-docs/issues/935

### 6.3 SharePoint Download Size Mismatch (Discussion #1667)

When downloading files from SharePoint, the API may report an incorrect file size. The downloaded file is complete and correct, but its actual size differs from the API-reported size. This causes download validation to fail. The workaround is `disable_download_validation`.

### 6.4 Invalid Hash on Deleted Items

Deleted items in delta responses sometimes include a placeholder hash (e.g., all-zeros QuickXorHash: `AAAAAAAAAAAAAAAAAAAAAAAAAAA=`) that does not correspond to any actual file content. This hash is useless for integrity verification. The reference implementation ignores server-provided hashes for deleted items and instead compares the local file hash against the previously stored database hash.

### 6.5 Rename/Move in Standalone Mode Causes Delete + Re-upload (Issues #876, #2579)

When the client runs in standalone mode (`--sync` as a one-shot), it cannot track filesystem events between runs. Renaming or moving a file or folder between sync runs is interpreted as a deletion at the old path followed by a creation at the new path. This causes the entire file to be deleted from OneDrive and re-uploaded. The only workaround is to use monitor mode (`--monitor`), which tracks inotify events in real time.

### 6.6 Authentication Code Invalidation (AADSTS70000)

Browser extensions (ad-blockers, URL sanitizers, tracking-parameter removers) can modify the OAuth redirect URI, invalidating the authorization code. This is not a client bug but a common user-facing issue. Authorization codes are single-use and short-lived. The workaround is to use a private/incognito browser window with extensions disabled.

### 6.7 Trailing Dot in Filenames (Issue #2678)

OneDrive rejects filenames ending with a dot (`.`), but this restriction is not documented in Microsoft's official naming restrictions page. The reference implementation includes trailing-dot detection in its name validation regex.

---

## 7. Cross-Platform Issues

### 7.1 Dual-Boot Windows/Linux and Files On-Demand

When dual-booting Windows and Linux with a shared OneDrive folder, Windows' "Files On-Demand" feature replaces actual file content with 0-byte reparse point stubs. These stubs are non-functional under Linux. The workaround is to disable "Save space and download files as you use them" in the Windows OneDrive client settings before using the same folder under Linux. Even after disabling, reparse point metadata may persist, requiring the `ntfs-3g-onedrive` plugin.

### 7.2 Character Encoding (UTF-8 and Validation)

The reference implementation validates all incoming strings for valid UTF-8 encoding. Invalid UTF-8 sequences in filenames or API responses are detected and logged as errors. The sync client should assume that all local filenames are UTF-8 (standard on modern Linux) but must handle cases where they are not.

**NFC/NFD normalization** is a concern on macOS, which uses NFD (decomposed) form for filenames on APFS/HFS+, while Linux and Windows typically use NFC (composed) form. OneDrive does not normalize, so a filename created on macOS may appear as a different byte sequence than the same visually-identical name created on Linux. A sync client operating on macOS must normalize filenames before comparison.

### 7.3 File Permission Models

OneDrive has no concept of POSIX file permissions. The reference implementation applies configured permission masks (`sync_dir_permissions`, `sync_file_permissions`) to all newly created files and directories. Existing permissions are not modified. Permission bits from the local filesystem are not preserved when uploading.

### 7.4 Case Sensitivity Differences

This is extensively covered in section 2.4. To summarize:
- **Linux:** Case-sensitive. `File.txt` and `file.txt` are different files.
- **macOS:** Case-insensitive by default (APFS). `File.txt` and `file.txt` are the same file.
- **Windows:** Case-insensitive (NTFS). `File.txt` and `file.txt` are the same file.
- **OneDrive:** Case-insensitive namespace.

A Linux-based sync client must enforce OneDrive's case-insensitive namespace constraints on the local filesystem, detecting and flagging conflicts where two local items would collide under case-insensitive rules.

### 7.5 Path Separator Handling

OneDrive API paths use forward slashes (`/`). The reference implementation normalizes all paths using its platform's `buildNormalizedPath()` function. A cross-platform client must ensure consistent path separator usage when constructing API paths vs. local filesystem paths.

---

## 8. Sync Logic Pitfalls

### 8.1 Race Between Local Scan and Remote Delta

There is an inherent race condition between scanning the local filesystem for changes and fetching remote delta responses. A file could be:
- Modified locally after the local scan but before the remote delta is applied
- Deleted remotely after the delta is fetched but before the local scan processes it

The reference implementation mitigates this by:
- Using inotify (in monitor mode) for real-time local change detection
- Processing all remote delta items before performing local uploads
- Using database state as the authoritative "last known good" for conflict resolution

### 8.2 Clock Skew Between Client and Server

The reference implementation compares timestamps between the local filesystem and OneDrive. Clock skew between the client machine and Microsoft's servers can cause spurious change detection. The mitigation is:
- Always using UTC for comparisons
- Truncating fractional seconds (OneDrive has whole-second precision)
- Using hash comparison as the definitive content-change check, not timestamps alone

### 8.3 OneDrive's "Processing" State for Files

After upload, OneDrive may take time to process a file (generate thumbnails, extract metadata, index content). During this processing window, the file's reported properties may be incomplete or in flux. The sync client should not immediately re-query an item after upload and expect stable metadata.

### 8.4 Eventual Consistency Delays

OneDrive is an eventually-consistent system. Changes made via the API may not immediately appear in delta responses. The reference implementation handles this by:
- Not assuming a just-created item will appear in the next delta query
- Using the saved deltaLink to track the "last known checkpoint" rather than relying on wall-clock time
- Supporting webhook notifications (see section 8.6) for faster change detection

### 8.5 Big Delete Detection

If a large number of items would be deleted (e.g., due to an unmounted volume appearing empty, or accidental bulk deletion), the sync engine must detect this and halt. The reference implementation:
- Counts the number of items that would be deleted online
- Compares against a configurable threshold (`classify_as_big_delete`, default 1000)
- Refuses to proceed without `--force` if the threshold is exceeded
- `--force` and `--resync` cannot be used together, because resync destroys the database, making it impossible to detect big deletes

### 8.6 Webhook Configuration Complexity

Webhook-based notifications require:
- A publicly reachable HTTPS endpoint with a valid certificate
- HTTPS only (since March 2023, Microsoft rejects HTTP webhook URLs)
- A reverse proxy (e.g., nginx) in front of the client's local listener (default port 8888)
- Proper TLS 1.2/1.3 configuration
- Firewall rules allowing inbound traffic from Microsoft's IP ranges
- Regular SSL certificate renewal (e.g., via Let's Encrypt certbot)
- SELinux may block nginx-to-localhost communication, requiring `httpd_can_network_connect` boolean

Webhook subscriptions have an expiration time and must be renewed periodically.

### 8.7 Multiple Account / Configuration Isolation

Each OneDrive account or SharePoint library requires:
- Its own configuration directory
- Its own sync directory (to prevent cross-pollination of data)
- Its own systemd service file (if running as a service)
- Its own authentication (refresh token)

The refresh token can be shared across Docker containers using the same OneDrive credentials, but separate accounts require separate authentication flows.

### 8.8 Configuration Changes Require Resync

Many configuration changes invalidate the local database state and require a full resync (`--resync`). These include:
- Changing `drive_id`
- Enabling/disabling `sync_business_shared_items`
- Changing `skip_dir`, `skip_file`, `skip_dotfiles`, `skip_symlinks`, `skip_size`
- Changing `check_nosync`

The reference implementation detects when these values differ from the last successful run and requires `--resync` before proceeding. CLI overrides of config file options also trigger this requirement.
