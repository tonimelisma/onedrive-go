# Microsoft Graph API Bugs & Quirks (from abraunegg/onedrive issues)

This document catalogs known Microsoft Graph API bugs and inconsistencies
discovered through the abraunegg/onedrive issue tracker. Each entry documents
the problem, root cause, impact, workaround, and implications for our
implementation.

---

## 1. driveId Casing Inconsistency Across API Endpoints

**Issue:** [#3336](https://github.com/abraunegg/onedrive/issues/3336)
**Status:** Fixed in v2.5.7
**Account Type:** OneDrive Personal

### Description

When creating a new folder on OneDrive via the API, the response returns a
`driveId` with different casing than what was originally stored in the local
database. For example, the default drive ID is `d8ebbd13a0551216` (lowercase),
but after creating a folder, the API response for `parentReference.driveId`
may return `D8EBBD13A0551216` (uppercase). This mismatch causes FOREIGN KEY
constraint failures when inserting the newly created item into a SQLite
database that uses case-sensitive string matching on the driveId column.

### Root Cause

**API bug.** The Microsoft Graph API is inconsistent in the casing of
`driveId` values across different API endpoints and response contexts. The
`/delta` endpoint, folder creation responses, and item metadata responses can
all return the same driveId in different cases (uppercase vs lowercase). This
was exacerbated by Microsoft's migration of Personal accounts to a new backend
platform that introduced the `sea8cc6beffdb43d7976fbc7da445c639` root ID
format, where API responses began returning uppercase driveIds that were
previously lowercase.

### Impact on Sync Operations

- FOREIGN KEY constraint failures when inserting items into the sync database
- Database consistency check failures requiring `--resync`
- Folder creation succeeds on OneDrive but the local database record is broken
- Subsequent syncs fail until database is rebuilt

### Workaround Used by Reference Implementation

The reference implementation normalizes all driveId values to a consistent
case (lowercase) before storing them in the database and before performing
any database lookups. All comparisons involving driveId are performed
case-insensitively.

### Implications for Our Implementation

- **Always normalize driveId values to lowercase** before storage and comparison
- Use `strings.ToLower()` on all driveId values received from any API response
- Database schema should either store normalized values or use
  case-insensitive collation for driveId columns
- This applies to all fields that contain driveId values: `parentReference.driveId`,
  `remoteItem.parentReference.driveId`, the drive ID in item IDs, etc.

---

## 2. 15-Character driveId (Truncated Leading Zero)

**Issue:** [#3072](https://github.com/abraunegg/onedrive/issues/3072)
**Related:** [#3115](https://github.com/abraunegg/onedrive/issues/3115), [#3136](https://github.com/abraunegg/onedrive/issues/3136), [#3169](https://github.com/abraunegg/onedrive/issues/3169), [#3165](https://github.com/abraunegg/onedrive/issues/3165), [#3180](https://github.com/abraunegg/onedrive/issues/3180)
**Status:** Fixed in v2.5.5 / v2.5.6
**Account Type:** OneDrive Personal
**Microsoft API Bug:** [onedrive-api-docs#1890](https://github.com/OneDrive/onedrive-api-docs/issues/1890)

### Description

OneDrive Personal driveId values are 16 hexadecimal characters. When a
driveId starts with `0`, the API sometimes drops the leading zero, returning
a 15-character driveId. This is an intermittent API bug that occurs in
specific response contexts, particularly when dealing with Shared Folders
between Personal accounts.

For example:
- Correct driveId: `024470056f5c3e43` (16 characters)
- Buggy driveId: `24470056f5c3e43` (15 characters)

The SharePoint URL ID in the same response correctly includes the leading
zero (`024470056f5c3e43`), proving the 15-character value is a bug.

### Root Cause

**API bug.** The Microsoft Graph API internally treats Personal driveId
values as numeric at some layer, which causes leading zeros to be stripped.
This is a classic integer-to-hex-string conversion bug where the output is
not zero-padded to the expected 16-character width. The bug appears in:

- `parentReference.driveId` fields
- Shared Folder metadata responses
- Delta query responses for shared folder items

The correct 16-character value appears in:
- SharePoint URL IDs
- Root item queries for the same drive
- The `driveId` portion of item IDs (which use uppercase and maintain the zero)

### Impact on Sync Operations

- **Application crashes** (assertion failures) when attempting to calculate
  filesystem paths for items with 15-character driveIds
- **Database consistency failures** because items are stored with mismatched
  driveId lengths (15 vs 16 characters)
- FOREIGN KEY constraint failures
- Shared Folders completely fail to sync
- The `--resync` flag cannot fix the issue because the API keeps returning
  the truncated value
- This was reported by many users across issues #3072, #3115, #3136, #3165,
  #3169, and #3180, all stemming from the same root cause

### Workaround Used by Reference Implementation

The reference implementation detects when a driveId is less than 16 characters
and performs a corrective API call (`getDriveIdRoot`) to fetch the correct
full-length driveId. It then zero-pads or replaces the truncated value before
storing it in the database. The fix went through multiple iterations:

1. Initial detection of < 16 character driveId values
2. Fetching correct driveId from a secondary API call
3. Normalizing all driveId comparisons to handle both 15 and 16 character forms
4. Additional handling for uppercase/lowercase inconsistencies compounded by
   this same bug

### Implications for Our Implementation

- **Validate driveId length on every API response.** If a Personal account
  driveId is less than 16 characters, left-pad with zeros to 16 characters
- Consider storing a normalized form: `strings.ToLower()` + zero-pad to 16 chars
- When comparing driveIds, normalize both sides before comparison
- The item ID prefix (e.g., `24470056F5C3E43!671`) may also contain the
  truncated form, so extract driveId from item IDs with caution
- Test specifically with Personal accounts whose driveId starts with `0`
- This bug is documented in Microsoft's API docs issue tracker but may not be
  fixed, so defensive coding is required permanently

---

## 3. Application Crash Due to 15-Character driveId Bug

**Issue:** [#3115](https://github.com/abraunegg/onedrive/issues/3115)
**Status:** Fixed in v2.5.5
**Account Type:** OneDrive Personal

### Description

This is a tracking issue for the crash consequence of bug #2 above. When the
sync engine attempts to compute the local filesystem path for a Shared Folder
item, it needs to walk the parent chain in the database. If the driveId is
15 characters (truncated), the database lookup fails, and the path computation
hits an assertion failure, crashing the application.

The crash manifests as:
```
core.exception.AssertError@src/itemdb.d(892): Assertion failure
```

### Root Cause

**Both API bug and client bug.** The API provides invalid 15-character driveIds
(API bug), and the client did not have defensive handling for unexpected
driveId lengths (client bug). The assertion assumed all driveIds would be
exactly 16 characters.

### Impact on Sync Operations

- Complete application crash
- Systemd service enters restart loop
- No sync possible until the bug is worked around
- Multiple users reported this across different sub-issues

### Workaround Used by Reference Implementation

- Removed hard assertions on driveId length
- Added detection and correction of truncated driveIds before path computation
- Added graceful error handling instead of assertions when driveId lookups fail
- The `force_children_scan` config option was introduced as a temporary
  workaround to bypass `/delta` queries that surface the bug

### Implications for Our Implementation

- Never use assertions or panics for data validation on API responses
- Use error returns and graceful degradation
- Validate all API response data defensively
- Have a fallback path computation strategy when database lookups fail
- Log warnings when API inconsistencies are detected, but continue operating

---

## 4. Personal Shared Folders Inconsistent API Response Values

**Issue:** [#3136](https://github.com/abraunegg/onedrive/issues/3136)
**Status:** Fixed in v2.5.5
**Account Type:** OneDrive Personal

### Description

When syncing OneDrive Personal Shared Folders, the API responses contain
inconsistent values across different endpoints. The `/delta` response for
a Shared Folder returns a 15-character driveId (truncated), while the
direct item query for the same Shared Folder root returns the correct
16-character driveId. This inconsistency causes the sync engine to store
mismatched records in its database, leading to assertion failures.

Additionally, the `parentReference.id` field within Shared Folder items
sometimes uses uppercase driveId prefixes (e.g., `24470056F5C3E43!671`)
while the `parentReference.driveId` is lowercase and truncated
(`24470056f5c3e43`).

### Root Cause

**API bug.** Multiple API inconsistencies compound in Shared Folder scenarios:

1. DriveId truncation (15 vs 16 characters)
2. Case inconsistency (uppercase in item IDs, lowercase in driveId fields)
3. Different endpoints returning different values for the same logical entity

### Impact on Sync Operations

- Shared Folders completely fail to sync
- Assertion failures and crashes
- Even `--resync` cannot fix the issue because the API keeps providing
  inconsistent data
- Users with large numbers of files (277,935+ items in one reported case)
  experience crashes after processing all items

### Workaround Used by Reference Implementation

The fix involved normalizing all driveId values received from the API:
- Case normalization (lowercase)
- Length validation and zero-padding
- Consistent normalization at the point of database insertion
- Additional API calls to fetch correct driveId when truncation is detected

### Implications for Our Implementation

- Shared Folder syncing is the most fragile area of the OneDrive API
- Build a normalization layer that processes all API responses before they
  reach the sync engine
- Create a `normalizedriveId(string) string` function used everywhere
- Test Shared Folder sync scenarios with driveIds starting with `0`
- Consider keeping a mapping table of known driveId aliases (15-char to 16-char)

---

## 5. FOREIGN KEY Constraint Failed (Cascading Database Errors)

**Issue:** [#3169](https://github.com/abraunegg/onedrive/issues/3169)
**Status:** Fixed in v2.5.6
**Account Type:** OneDrive Personal

### Description

Every directory creation or item insertion into the local database triggers a
FOREIGN KEY constraint failure. This occurs because the parent record's
driveId does not match the child record's driveId due to the truncation and
casing bugs described above. The error cascades: once one record has an
inconsistent driveId, all child records fail to insert.

The user reported 13,812 directories needed to be created, and every single
one triggered the FOREIGN KEY error.

### Root Cause

**API bug causing client database corruption.** The underlying issue is the
same driveId inconsistency (15 vs 16 characters, uppercase vs lowercase),
but the manifestation is different: instead of an assertion crash, the
database's referential integrity constraints catch the mismatch.

### Impact on Sync Operations

- Complete inability to sync any new items
- Every directory creation fails with FOREIGN KEY error
- Database checkpoint and vacuum operations also fail
- `--resync` does not help because the API keeps providing inconsistent data
- The application becomes completely non-functional

### Workaround Used by Reference Implementation

Same normalization fixes as bugs #2-#4. Additionally:
- The database layer was made more resilient to handle FOREIGN KEY errors
  gracefully instead of failing the entire operation
- Better error recovery: when a FOREIGN KEY error occurs, attempt to fix the
  parent record before retrying

### Implications for Our Implementation

- Design the database schema to minimize cascading failures
- Consider using normalized driveIds as the canonical form everywhere
- Implement a database self-healing mechanism that can detect and correct
  driveId mismatches
- Use database transactions so partial failures can be rolled back cleanly
- Log FOREIGN KEY errors with full context (both the failing record and the
  expected parent) for debugging

---

## 6. Database Consistency Issue on Clean Setup

**Issue:** [#3165](https://github.com/abraunegg/onedrive/issues/3165)
**Status:** Fixed in v2.5.6
**Account Type:** OneDrive Personal

### Description

Even on a completely fresh installation with a clean database, the very first
sync (or second sync after initial download) triggers a database consistency
error. Reverting to v2.5.4 fixes the issue, confirming this is not a database
corruption issue but rather a data processing issue introduced when the client
began handling the new `sea8cc6beffdb43d7976fbc7da445c639` root ID format.

### Root Cause

**API bug combined with new backend migration.** Microsoft migrated Personal
accounts to a new backend platform, introducing the
`sea8cc6beffdb43d7976fbc7da445c639` root ID format. This migration changed
how API responses are structured:

1. DriveId values in responses switched from lowercase to uppercase
2. The `/delta` endpoint stopped returning Shared Folder data after the
   migration
3. The root item ID format changed entirely

The reference implementation's v2.5.5 release attempted to handle these
changes but introduced a regression where the case normalization was
incomplete, causing database consistency failures even on fresh setups.

### Impact on Sync Operations

- Fresh installations immediately fail
- No workaround available for users on the affected client version
- Rolling back to v2.5.4 was the only option until a fix was released

### Workaround Used by Reference Implementation

- Comprehensive case normalization across all API response handling
- The `force_children_scan` configuration option was added to bypass the
  broken `/delta` endpoint for Shared Folders
- Filed API bug [onedrive-api-docs#1891](https://github.com/OneDrive/onedrive-api-docs/issues/1891)
  for the missing Shared Folder data in `/delta` responses

### Implications for Our Implementation

- The `sea8cc6beffdb43d7976fbc7da445c639` root ID is now the standard
  format for Personal accounts on the new backend
- Must handle both old-format and new-format root IDs
- The `/delta` endpoint may not include Shared Folder items; a separate
  enumeration strategy (using `/children` API) may be needed as a fallback
- Test against both old-format and new-format Personal accounts
- API behavior can change without notice due to backend migrations

---

## 7. 308 Permanent Redirect Without Location Header (Webhook Subscription)

**Issue:** [#3167](https://github.com/abraunegg/onedrive/issues/3167)
**Status:** Fixed (crash prevented; underlying API bug unresolved)
**Account Type:** OneDrive Personal
**Microsoft API Bug:** [onedrive-api-docs#1895](https://github.com/OneDrive/onedrive-api-docs/issues/1895)

### Description

When the application attempts to renew a webhook subscription (via a PATCH
request to `/v1.0/subscriptions/{id}`), the Microsoft Graph API responds
with HTTP 308 (Permanent Redirect) but does not include a `Location` header.
RFC 7538 requires that a 308 response include a `Location` header indicating
the new URI. Without it, the client has no way to follow the redirect.

The response headers are:
```
HTTP/2 308
cache-control: no-cache
strict-transport-security: max-age=31536000
request-id: ...
content-length: 0
```

No `Location` header is present.

### Root Cause

**API bug.** This is a protocol violation by the Microsoft Graph API. The 308
status code is being misused: either the API should return a proper 308 with
a Location header, or it should return a different status code (400, 404, etc.)
if the subscription cannot be renewed. This bug was associated with Personal
accounts on the new backend (indicated by the `sea8cc6beffdb43d7976fbc7da445c639`
root ID) and was observed specifically in the West Europe data center.

### Impact on Sync Operations

- Application crash when attempting to parse the empty response body as JSON
  (`std.json.JSONException: JSONValue is not an object`)
- Webhook-based real-time sync completely broken
- Monitor mode crashes and enters a restart loop
- Disabling webhooks (`webhook_enabled = false`) is the only immediate
  workaround

### Workaround Used by Reference Implementation

- Added graceful handling of 308 responses without Location headers
- If a webhook renewal fails with 308, the subscription is deleted and
  recreated from scratch
- The crash on empty JSON response was fixed by checking response validity
  before parsing

### Implications for Our Implementation

- **Handle all HTTP redirect codes defensively**, including 308 without a
  Location header
- Never assume an HTTP response body is valid JSON without checking status
  code and content-length first
- Webhook subscription renewal should be wrapped in retry logic with
  fallback to subscription recreation
- Consider implementing a "delete and recreate" strategy as the primary
  renewal approach rather than PATCH
- The Microsoft Graph API can return non-standard HTTP responses; our HTTP
  client layer must be resilient to protocol violations
- Data center-specific bugs exist; do not assume uniform behavior across
  Microsoft's infrastructure

---

## 8. Case-Insensitive Match Between Different Files (POSIX Compliance)

**Issue:** [#3237](https://github.com/abraunegg/onedrive/issues/3237)
**Status:** Fixed in v2.5.6
**Account Type:** Business / Office365

### Description

When uploading a file, the client checks if a file with the same name (case-
insensitive) already exists at the online path. The API incorrectly returns
data for a completely different file in a different path. For example:

- Local file to upload: `.git/refs/tags/v1.0.0`
- API reports a case-insensitive match with: `v2.0.0` (a different file in
  an `_api` subdirectory)

The error message is:
```
POSIX 'case-insensitive match' between 'v1.0.0' (local) and 'v2.0.0' (online)
which violates the Microsoft OneDrive API namespace convention
```

These are clearly different filenames (`v1.0.0` vs `v2.0.0`), and the fact
that the API returns a match at all suggests an API-level search bug.

### Root Cause

**API bug.** The Microsoft Graph API's "get item by path" endpoint appears
to perform a fuzzy or overly broad case-insensitive search that can return
items from different paths or with different names than what was queried.
This is particularly problematic for git repositories where files like
`v1.0.0`, `v2.0.0`, `v1.0.0a` exist in nearby directory structures.

Additionally, there is a known OneDrive behavior where the filesystem is
case-insensitive: two files that differ only in case cannot coexist in the
same folder. But this bug goes further by matching files with completely
different names.

The issue was difficult to reproduce because it did not manifest in `--resync`
mode (which uses a different code path) but appeared consistently in normal
sync mode.

### Impact on Sync Operations

- Files are incorrectly skipped during upload with a misleading error
  about POSIX case-insensitive matches
- Data is not uploaded to OneDrive even though there is no actual conflict
- Users with git repositories synced to OneDrive are particularly affected
  because of the many similarly-named ref files

### Workaround Used by Reference Implementation

- Improved the case-insensitive match comparison to check that the files
  are actually in the same directory path, not just that a similarly-named
  file exists somewhere
- Added more detailed debug logging to show exactly what the API returned
  versus what was queried
- The comparison now validates both the filename AND the parent path

### Implications for Our Implementation

- **OneDrive is fundamentally case-insensitive** for filenames within the
  same folder; our client must handle this
- When checking for existing files before upload, compare the full path
  (including parent directory), not just the filename
- The API's "get item by path" results should be validated to ensure the
  returned item actually matches the queried path
- Be cautious with git repository syncing; `.git` directories contain
  many files that may trigger false case-insensitive conflicts
- Consider adding a warning/skip for known problematic patterns

---

## 9. Perpetual --resync Loop

**Issue:** [#3180](https://github.com/abraunegg/onedrive/issues/3180)
**Status:** Fixed (duplicate of #3115)
**Account Type:** OneDrive Personal

### Description

The application constantly requests `--resync` even when `--resync` has
already been provided. The sync never completes; instead, it immediately
detects "an application cache state issue" and exits, even on a freshly
rebuilt database.

```
Deleting the saved application sync status ...
...
An application cache state issue has been detected where a --resync is required
```

### Root Cause

**API bug causing client-side detection failure.** The user's driveId starts
with `0` (`09d486f24e607ac6`), making them susceptible to the 15-character
driveId truncation bug. After the `--resync` rebuilds the database and
fetches fresh data from the API, the very first delta response contains
truncated driveIds. The database consistency check immediately detects the
mismatch and flags it as a cache state issue, requesting another `--resync`.
This creates an infinite loop.

### Impact on Sync Operations

- Complete inability to use the application
- First-time setup fails
- The `--resync` flag is supposed to be the nuclear option that fixes
  everything, but even it cannot recover from this API bug

### Workaround Used by Reference Implementation

The fix was the same driveId normalization that resolved #3072/#3115. Once
the client normalizes all driveIds (zero-padding, lowercasing) before storage
and comparison, the consistency check passes.

### Implications for Our Implementation

- The "resync" mechanism must be robust against persistent API bugs
- If the API consistently returns bad data, resync will not help unless
  the client normalizes the data
- Design the database consistency check to be tolerant of known API
  inconsistencies rather than failing hard
- Consider a "known API quirks" normalization pass that runs on all
  incoming data before any validation

---

## 10. File Download Size and Hash Mismatch with iOS .heic Files

**Issue:** [#2471](https://github.com/abraunegg/onedrive/issues/2471)
**Status:** Closed (unresolved API bug, labeled "Unresolved")
**Account Type:** OneDrive Personal
**Microsoft API Bug:** [onedrive-api-docs#1723](https://github.com/OneDrive/onedrive-api-docs/issues/1723)

### Description

When downloading `.heic` files that were uploaded from iOS devices via the
OneDrive iOS app, the file metadata (size and hash) reported by the API does
not match the actual data delivered during download. Specifically:

- The JSON metadata reports a file size of X bytes
- The actual download delivers Y bytes (significantly smaller)
- The hash naturally does not match either

Example from debug logs:
- API reported size: **3,039,814 bytes**
- Actual downloaded size: **682,474 bytes**

For larger files (>4MB) that use session-based downloads, the same issue
manifests: the JSON reports 6,887,427 bytes, but the download session
reports only 4,583,414 bytes available, and that is what gets delivered.

### Root Cause

**API bug.** OneDrive performs server-side processing on iOS-uploaded `.heic`
files (likely HEIC-to-JPEG conversion or metadata stripping). The API
metadata (size, hash) reflects the original uploaded file, but the download
endpoint serves a modified/converted version of the file. This means:

1. The metadata is from the original iOS upload
2. The download content is a post-processed version
3. There is no way for the client to know which version it will receive

This bug has been open since August 2023 and was filed with Microsoft but
remained unresolved through at least October 2025. It specifically affects
`.heic` files uploaded from iOS devices; `.heic` files uploaded via the web
or from Linux do not exhibit this behavior.

### Impact on Sync Operations

- `.heic` files from iOS uploads fail download validation
- Files are deleted locally after failed integrity checks
- Users must use `--disable-download-validation` to work around the issue,
  sacrificing data integrity verification
- **Actual data loss**: the downloaded file is smaller than the original,
  meaning image data is being lost/modified server-side

### Workaround Used by Reference Implementation

- Added a `--disable-download-validation` flag that skips size and hash
  checks after download
- Fixed a secondary bug where the hash comparison was comparing QuickXorHash
  with SHA256 values (a logging/comparison bug in the validation code)
- No actual fix possible since the API itself delivers different data than
  what it advertises

### Implications for Our Implementation

- **Download validation must be configurable** - allow users to disable it
  for known-broken file types
- Consider a more granular approach: skip validation only for `.heic` files,
  or only when the size mismatch pattern matches the known iOS conversion bug
- When a size mismatch is detected, log it clearly as a known API issue
  rather than a generic "corruption" error
- The QuickXorHash is the canonical hash for Personal accounts; do not mix
  hash types during validation
- Consider accepting the downloaded file if the download itself completed
  without HTTP errors, even if the metadata does not match, with an
  appropriate warning
- This bug demonstrates that file metadata and file content can be
  inconsistent on OneDrive; never rely solely on metadata for integrity

---

## 11. Not Syncing Shared Items Between Personal Accounts

**Issue:** [#2957](https://github.com/abraunegg/onedrive/issues/2957)
**Status:** Closed
**Account Type:** OneDrive Personal

### Description

Shared folders between Personal accounts do not appear in `/delta` responses
at all, despite being visible in the web interface and other clients. The
Shared Folder "link" appears in the user's root, but its contents are not
enumerated. When the only content in a Personal account is a Shared Folder,
the API returns zero items to process.

### Root Cause

**API bug / account-level issue.** This manifests in two scenarios:

1. **Account-level issue:** Some Personal accounts, particularly those
   serviced by the Germany West Central data center, simply do not return
   Shared Folder data via the delta API. Microsoft needs to be contacted
   directly to resolve account-level issues.

2. **API path encoding bug:** When Shared Folder names contain spaces, the
   API returns HTML-encoded paths (e.g., `20150107%20-%20Passbild` instead
   of `20150107 - Passbild`) in `parentReference.path` fields. This breaks
   path matching and prevents files from being downloaded.
   See [onedrive-api-docs#1765](https://github.com/OneDrive/onedrive-api-docs/issues/1765).

3. **Shared Folder database tie record:** The sync engine needs to create
   a "tie record" linking the Shared Folder's remote driveId to the local
   database. If this record is not created (due to API data issues), the
   entire Shared Folder tree becomes invisible to the sync engine.

### Impact on Sync Operations

- Shared Folders appear empty (only directories, no files)
- The delta API completely omits Shared Folder content in some cases
- URL-encoded spaces in path names prevent path matching
- Files are enumerated but never downloaded

### Workaround Used by Reference Implementation

- Added a `Prefer: deltashowremoteitemsaliasid` HTTP header for Personal
  accounts to force the API to include remote/shared items in delta responses
  (previously only used for Business accounts)
- Added detection and URL-decoding of `%20` and similar HTML entities in
  path fields from API responses
- Implemented the `force_children_scan` option to use `/children` API calls
  instead of `/delta` as a fallback when delta responses are incomplete

### Implications for Our Implementation

- **Always include the `Prefer: deltashowremoteitemsaliasid` header** in
  delta requests for Personal accounts
- URL-decode all path fields received from the API (`parentReference.path`)
- The `/delta` endpoint is not reliable for Shared Folder enumeration on
  Personal accounts; implement a fallback using `/children` traversal
- The `remoteItem` field in a drive item indicates a Shared Folder link;
  the actual content lives on a different drive
- For Shared Folders, the sync engine must track items across multiple
  driveIds and maintain cross-drive references in the database
- Account-level issues exist that are outside our control; provide clear
  error messages directing users to contact Microsoft support

---

## 12. Not All Files Being Downloaded (Shared Folder Path Issues)

**Issue:** [#2562](https://github.com/abraunegg/onedrive/issues/2562)
**Status:** Fixed in v2.5.0
**Account Type:** OneDrive Personal

### Description

Shared folders between Personal accounts sync only empty directories; the
files within them are not downloaded. The `/delta` response correctly
enumerates all items (2,836 items in one case), but when the sync engine
processes these items, it cannot resolve the parent path for Shared Folder
items, causing them to be skipped.

The key error in the logs:
```
[DEBUG] Parent ID is not in DB ..
[DEBUG] The following generated a broken tree query:
[DEBUG] Drive ID: f3028bd758552faf
[DEBUG] Item ID: F3028BD758552FAF!313
ERROR: A database consistency issue has been caught
```

### Root Cause

**API bug and client architectural limitation.** Multiple API issues compound:

1. **Missing parent records:** The `/delta` response for a Shared Folder
   references parent IDs that are on the *other* user's drive, not in the
   local database. The client needs to synthesize or fetch these parent records.

2. **URL-encoded paths:** The API returns `%20` for spaces in
   `parentReference.path`, breaking path matching and sync_list evaluation.
   See [onedrive-api-docs#1765](https://github.com/OneDrive/onedrive-api-docs/issues/1765).

3. **Array bounds error:** The sync engine assumed a specific structure for
   Shared Folder path components, leading to an index-out-of-bounds crash
   when the path structure differed from expectations.

### Impact on Sync Operations

- Shared Folders appear to sync (directories are created) but contain no files
- Large numbers of items are fetched from the API but all discarded
- Database consistency check fails after the initial sync attempt

### Workaround Used by Reference Implementation

- Rewrote the Shared Folder parent path resolution to handle cross-drive
  references
- Added URL-decoding for `%20` in paths
- Fixed the array bounds error when parsing Shared Folder path components
- Implemented `force_children_scan` as a fallback enumeration strategy
- The fix was a significant rewrite in the alpha/v2.5.0 series

### Implications for Our Implementation

- **Shared Folders require a fundamentally different data model** than
  regular sync items. They span multiple drives and have cross-drive parent
  references.
- The database must support items from multiple driveIds with proper
  foreign key relationships across drives
- Parent path resolution for Shared Folders cannot rely on walking the
  parent chain in a single drive's items; it must handle cross-drive
  references
- URL-decode all `parentReference.path` values
- The `/children` endpoint is more reliable than `/delta` for initial
  Shared Folder enumeration
- Consider a separate sync workflow for Shared Folders vs. own-drive items

---

## Summary: Key Takeaways

### Critical API Inconsistencies to Handle

| Issue | API Inconsistency | Normalization Required |
|-------|-------------------|----------------------|
| #3336, #3115 | driveId casing (upper/lower) | `strings.ToLower()` on all driveIds |
| #3072, #3115 | driveId truncation (15 vs 16 chars) | Left-pad with `0` to 16 characters |
| #3115, #3136 | Item ID prefix casing vs driveId casing | Normalize independently |
| #3237 | Case-insensitive match returns wrong file | Validate full path, not just name |
| #2562, #2957 | URL-encoded spaces in path fields | URL-decode `parentReference.path` |
| #2471 | File metadata vs actual download content | Configurable download validation |
| #3167 | HTTP 308 without Location header | Defensive HTTP response handling |

### Defensive Design Principles

1. **Normalize all identifiers on ingestion.** Every driveId received from
   any API response should be lowercased and zero-padded to 16 characters
   before storage or comparison.

2. **Never trust API metadata for file integrity.** The API can report one
   file size and deliver a different one. Download validation should be
   configurable and the failure mode should be a warning, not data deletion.

3. **Handle Shared Folders as a special case.** They involve cross-drive
   references, inconsistent driveIds, missing parent records, and unreliable
   delta responses. Plan for a separate code path.

4. **URL-decode path fields.** The API sometimes returns URL-encoded
   characters in `parentReference.path`. Always decode these.

5. **Handle HTTP protocol violations.** The Microsoft Graph API can return
   non-standard HTTP responses (308 without Location). The HTTP client layer
   must handle these gracefully.

6. **Case-insensitive filesystem semantics.** OneDrive is case-insensitive.
   Two files differing only in case cannot coexist in the same folder. Our
   client must enforce this locally and detect conflicts.

7. **Delta responses may be incomplete.** The `/delta` endpoint may not
   include all items, especially for Shared Folders after account migrations.
   Implement a `/children`-based fallback for complete enumeration.

8. **Database resilience.** Design the sync database to handle API
   inconsistencies without cascading failures. Use normalized keys, avoid
   hard assertions on API data, and implement self-healing for known
   quirk patterns.

### Priority Order for Implementation

1. **driveId normalization** (affects all Personal account operations)
2. **URL-decoding of path fields** (affects all accounts with spaces in names)
3. **Configurable download validation** (affects iOS users with .heic files)
4. **Shared Folder cross-drive support** (complex but required for full sync)
5. **Defensive HTTP handling** (affects webhook/real-time sync)
6. **Case-insensitive conflict detection** (affects Linux users primarily)
