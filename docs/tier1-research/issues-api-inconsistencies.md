# API Inconsistencies, Blockers & Data Integrity Risks

This document catalogs known Microsoft Graph API inconsistencies, bugs, and blockers
discovered through the abraunegg/onedrive project's issue tracker. Each issue is documented
with its technical root cause, impact severity, workaround (if any), and relevance to our
sync engine implementation.

**Sources:** 26 issues from abraunegg/onedrive (GitHub), spanning 2018--2025.

---

## 1. DriveId Bugs

### 1.1 Truncated DriveId -- Leading Zero Dropped (Issues #3072, #3115, #3336)

**Summary:** The Microsoft Graph API intermittently returns 15-character driveId values
instead of the expected 16 characters for OneDrive Personal accounts. The API drops the
leading zero from driveIds that start with `0`.

**Technical Details:**
- DriveIds are normally 16 hex characters (e.g., `024470056f5c3e43`).
- In some API responses (particularly delta responses and Shared Folder parent references),
  the leading zero is dropped, producing a 15-character value (`24470056f5c3e43`).
- The same driveId can appear as both 15 and 16 characters within a single sync session:
  the "root" details for a shared folder return the correct 16-character value, but the
  folder's parent reference in delta responses uses the truncated 15-character value.
- This was filed as Microsoft API bugs:
  [onedrive-api-docs#1890](https://github.com/OneDrive/onedrive-api-docs/issues/1890) and
  [onedrive-api-docs#1891](https://github.com/OneDrive/onedrive-api-docs/issues/1891).

**Impact:** Critical. Causes FOREIGN KEY constraint failures in the sync database because
the truncated driveId does not match the stored 16-character version. Results in assertion
failures, crashes, and database consistency errors that demand a full resync.

**Workaround:** Normalize all driveIds on ingestion. If a driveId is less than 16
characters, left-pad with zeros. The reference implementation added this workaround in
PR #3075 and PR #3116, which detects short driveIds and fetches the correct value via an
additional API call to `getDriveIdRoot`.

**Relevance to Our Implementation:** HIGH. We must normalize driveIds immediately upon
receiving any API response. All driveId comparisons and database lookups must use the
normalized (zero-padded) form. This should be a fundamental invariant enforced at the API
client layer, not scattered across sync logic.

### 1.2 DriveId Casing Inconsistency (Issue #3336)

**Summary:** Different Graph API endpoints return driveIds in different cases (lowercase
vs. uppercase), causing foreign key mismatches when the database uses case-sensitive
matching.

**Technical Details:**
- The account's default driveId may be returned as `d8ebbd13a0551216` (lowercase) by one
  endpoint, but as `D8EBBD13A0551216` (uppercase) in parentReference fields or root item
  IDs.
- When creating a folder online, the API response for the new item uses a different casing
  than what was stored during delta processing, triggering `FOREIGN KEY constraint failed`.
- The folder is actually created successfully on OneDrive; only the local database
  insertion fails.

**Impact:** High. Prevents new folder creation from being tracked in the local database.
Requires resync to recover.

**Workaround:** Case-normalize all driveIds (e.g., always store lowercase). The reference
implementation addressed this in PR #3335.

**Relevance to Our Implementation:** HIGH. All driveId values must be case-normalized
(recommend lowercase) at the API response parsing layer before storage or comparison.

---

## 2. Hash and Integrity Issues

### 2.1 Files Without Any Hash (Issues #2433, #2442)

**Summary:** The Graph API returns file items with no hash values at all -- no
quickXorHash, no sha1Hash, no sha256Hash. This was observed primarily on Business/Office365
accounts.

**Technical Details:**
- Zero-byte files consistently have no hash in API responses (confirmed root cause for
  many cases).
- Some non-zero files on SharePoint/Business accounts also lack hashes intermittently.
- Microsoft confirmed this is a known issue: for enumeration/delta scenarios, hash
  generation may be "too expensive" and is skipped for certain files on SharePoint.
- Without a hash, file content comparison falls back to timestamps only, which is
  unreliable for conflict detection.

**Impact:** Medium-High. Without hashes, the client cannot reliably detect whether a file
has actually changed, leading to unnecessary downloads/uploads or missed changes.

**Workaround:** Accept that zero-byte files will never have hashes (compute locally as a
known constant). For non-zero files missing hashes, fall back to size + timestamp
comparison. The reference implementation added handling in PR #2436 to skip the hash warning
for zero-byte files and treat missing hashes gracefully.

**Relevance to Our Implementation:** HIGH. Our hash comparison logic must handle the case
where the server provides no hash at all. For zero-byte files, we should compute the known
QuickXorHash constant (or simply match by size=0). For larger files with missing hashes, we
need a documented fallback strategy (likely size + mtime + eTag comparison).

### 2.2 Download Validation Failures with Azure Data Protection (Issue #3066)

**Summary:** Files protected by Azure Information Protection / Azure Data Protection fail
download validation checks because the downloaded content does not match the server-reported
hash and/or size.

**Technical Details:**
- The Graph API reports the file size and hash of the encrypted/protected version, but the
  actual downloaded bytes may differ (the download endpoint may serve a different
  representation).
- Affected file types include `.xlsb`, `.xlsm`, `.pptx` and other Office formats with
  Azure protection applied.
- Results in "File download size mismatch" and "File download hash mismatch" errors.
- The client deletes the locally downloaded file due to failed integrity checks.

**Impact:** Medium. Prevents syncing of Azure-protected Office files. Affects primarily
Business/Enterprise accounts.

**Workaround:** Use `--disable-download-validation` to bypass integrity checks for these
files. The downloaded files may not be openable on Linux anyway (they require Azure
Information Protection client to decrypt).

**Relevance to Our Implementation:** MEDIUM. We should support a per-file or per-folder
option to disable download validation. We should also log clearly when a validation failure
occurs and not silently delete files. Consider detecting Azure-protected files (they may
have specific metadata) and handling them as a special case.

---

## 3. Shared Folder Issues

### 3.1 Personal Shared Folders -- Inconsistent API Values (Issues #3127, #3136)

**Summary:** OneDrive Personal Shared Folders return inconsistent data between different
API calls, causing path computation failures and crashes.

**Technical Details:**
- When shared folders are processed during delta sync, the parentReference may use a
  truncated driveId (the leading-zero bug from Section 1.1).
- The `computePath` function fails because it cannot find the parent item in the database
  due to the driveId mismatch, triggering an assertion failure at `itemdb.d(892)`.
- Additionally, shared folders that were created recently (within the past year or so) may
  appear as `.url` shortcut files in Mac/Windows OneDrive clients, which is itself a
  Microsoft platform bug.
- The crash occurs during `checkJSONAgainstClientSideFiltering` when the client tries to
  determine the local filesystem path for items inside the shared folder.

**Impact:** Critical. Crashes the sync client entirely, preventing any sync from completing.
The crash occurs even on `--resync`, making recovery difficult.

**Workaround:** The reference implementation worked around this with the driveId
normalization fix (PR #3075, #3116). Users can also skip the problematic shared folder via
`skip_dir` configuration, though this is unreliable as the crash can occur before filtering
is applied.

**Relevance to Our Implementation:** HIGH. Shared folder support requires extremely
defensive programming. We must handle missing or inconsistent parent references gracefully
-- log the issue and skip the item rather than crashing. Path computation must tolerate
driveId mismatches by normalizing before lookup.

### 3.2 Business Shared Folders -- Cross-Organization Invisible (Issue #966)

**Summary:** Shared folders from users outside your organization are not visible via the
Graph API, even though they appear in the OneDrive web interface.

**Technical Details:**
- The Graph API's "shared with me" endpoint does not return items shared from external
  organizations on Business accounts.
- These items display with a "world" icon in the OneDrive web UI but simply do not exist
  in API responses.
- This is a fundamental Graph API limitation, not a client bug.

**Impact:** Medium. Users who collaborate across organizations cannot sync those shared
resources via any third-party client.

**Workaround:** None. This requires Microsoft to fix the Graph API.

**Relevance to Our Implementation:** LOW (for initial implementation). We should document
this limitation clearly. Cross-org shared folders are simply invisible to the API.

### 3.3 SharePoint Shortcuts / "Add to My Files" Not Supported (Issue #1224)

**Summary:** When users add SharePoint folders to their OneDrive via "Add Shortcut to My
Files," the Graph API does not expose these shortcuts in the user's drive enumeration.

**Technical Details:**
- The Windows/Mac OneDrive clients handle these shortcuts natively, but the Graph API does
  not return them as part of the user's drive items.
- The only workaround is to configure separate sync instances per SharePoint document
  library using explicit drive IDs.
- Filed as [onedrive-api-docs#1427](https://github.com/OneDrive/onedrive-api-docs/issues/1427)
  (later closed without resolution) and
  [onedrive-api-docs#1674](https://github.com/OneDrive/onedrive-api-docs/issues/1674).

**Impact:** Medium. A commonly-used OneDrive feature is inaccessible via the API.

**Workaround:** Configure multiple sync targets, each pointing to a specific SharePoint
document library by drive ID.

**Relevance to Our Implementation:** MEDIUM. We should support multi-drive configurations
to allow users to sync specific SharePoint libraries. Document the limitation around
shortcuts.

---

## 4. Database and State Integrity Issues

### 4.1 NOT NULL Constraint -- Missing Item Type (Issue #2938)

**Summary:** The Graph API sometimes returns items (particularly OneNote notebooks and their
constituent files) without the standard `file` or `folder` facets, causing database
insertion failures due to NOT NULL constraints on the `type` column.

**Technical Details:**
- OneNote notebooks are returned with a `package` facet instead of `file` or `folder`.
- The `package.type` field contains `"oneNote"`, but there is no `file` or `folder` key.
- The client's database schema expects every item to have a type (`file`, `folder`, or
  `remote`), and NULL values trigger `NOT NULL constraint failed: item.type`.
- OneNote `.one` and `.onetoc2` files also appear with inconsistent metadata.

**Impact:** Medium. Prevents sync from completing when OneNote notebooks are present in the
user's drive.

**Workaround:** The reference implementation added explicit handling to skip OneNote items
(PR #2939). They are detected by the presence of the `package` facet with
`type == "oneNote"`.

**Relevance to Our Implementation:** HIGH. Our data model must account for the `package`
item type. OneNote notebooks, and potentially other "package" types, should be explicitly
handled -- either synced as opaque folder structures or skipped with a clear user message.
Our database schema should allow a `package` type or handle it as a special folder.

### 4.2 FOREIGN KEY Constraint Failures (Issues #3169, #3336)

**Summary:** FOREIGN KEY constraint violations occur when inserting items whose parent
driveId does not match any existing record, due to the driveId truncation and casing bugs
described in Sections 1.1 and 1.2.

**Technical Details:**
- A new folder is created locally and uploaded to OneDrive. The API confirms creation.
- When the client tries to insert the item into the local database, the driveId in the API
  response does not match the driveId stored for the parent (due to case difference or
  leading-zero truncation).
- The FOREIGN KEY constraint on the parent relationship fails.
- The folder exists correctly on OneDrive; only the local state is broken.

**Impact:** High. Breaks incremental sync and requires a full resync to recover.

**Workaround:** DriveId normalization (Sections 1.1 and 1.2).

**Relevance to Our Implementation:** HIGH. This reinforces the need for driveId
normalization at the API layer. Our database design should also consider using less strict
constraints (e.g., deferred foreign keys) or handling orphaned items gracefully rather than
crashing.

### 4.3 Database Consistency Errors on Clean Setup (Issue #3165)

**Summary:** Even on a completely fresh installation with a new database, consistency errors
appear on the second sync, requiring another resync.

**Technical Details:**
- The first sync completes successfully and downloads all data.
- The second sync detects a "database consistency issue" and demands a resync.
- Root cause traced to the driveId truncation bug (#3072): on first sync, items are stored
  with their (possibly truncated) driveIds. On second sync, the delta response may use the
  correct (16-char) driveId, causing a mismatch.
- Reverting to v2.5.4 (which did not have enhanced consistency checks) appeared to "fix"
  the problem by not detecting the inconsistency.

**Impact:** High. Creates an infinite resync loop for affected accounts.

**Workaround:** Build from master with the driveId normalization fix (PR #3174).

**Relevance to Our Implementation:** HIGH. Our database consistency checks must be
resilient to driveId format variations. Consistency checking should normalize IDs before
comparison.

### 4.4 Repeated Database Consistency Errors with Shared Folders (Issue #3030)

**Summary:** Syncing Personal Shared Folders produces repeated database tree query failures,
with the broken query pointing to items in the shared folder whose driveId is 15 characters.

**Technical Details:**
- After processing the main drive's delta, the client fetches delta for each shared folder.
- The shared folder items reference a 15-character driveId.
- When the client builds the path tree, it cannot find the root of the shared folder
  because the stored driveId (16 chars) does not match the referenced driveId (15 chars).
- This produces "A database consistency issue has been caught" on every sync cycle.

**Impact:** High. Shared folders become completely un-syncable, and the error repeats on
every sync.

**Workaround:** DriveId normalization (PR #3047, #3174).

**Relevance to Our Implementation:** HIGH. Same as Section 1.1 -- driveId normalization is
the fundamental fix.

---

## 5. HTTP and Protocol Issues

### 5.1 HTTP 308 Redirect Without Location Header (Issue #3167)

**Summary:** The Graph API returns HTTP 308 (Permanent Redirect) responses without the
required `Location` header, making it impossible for the client to follow the redirect.

**Technical Details:**
- Observed when renewing webhook subscriptions via PATCH to
  `graph.microsoft.com/v1.0/subscriptions/{id}`.
- RFC 7538 requires a `Location` header with HTTP 308 responses.
- The Graph API response includes `content-length: 0` and no `Location` header.
- Occurs specifically for OneDrive Personal accounts that have been migrated to the new
  backend (identifiable by root IDs containing `sea8cc6beffdb43d7976fbc7da445c639`).
- Observed in the "West Europe" data center.
- Filed as [onedrive-api-docs#1895](https://github.com/OneDrive/onedrive-api-docs/issues/1895).

**Impact:** Medium. Prevents webhook subscription renewal, causing the client to crash when
running in monitor mode with webhooks enabled.

**Workaround:** The reference implementation added graceful handling of the 308 without
Location (PR #3172) -- the client catches the error and retries or falls back to polling.
Users can also disable webhooks (`webhook_enabled = false`).

**Relevance to Our Implementation:** MEDIUM. Our HTTP client layer must handle 308 responses
robustly: if no Location header is present, log the anomaly and fall back to non-redirect
behavior. Webhook renewal should not crash the entire application.

### 5.2 HTTP 400 on Business -- Invalid Delta Expression (Issue #237)

**Summary:** OneDrive Business returns HTTP 400 (Bad Request) when the delta endpoint is
called with a lowercase driveId, because the API expects a specific casing format.

**Technical Details:**
- The error message was: `The expression "drives('bc7d88ec1f539dcf')/items/BC7D88EC1F539DCF!107/delta" is not valid.`
- The delta API on SharePoint/Business interprets the driveId in a case-sensitive manner
  and rejects requests where the driveId casing does not match the server's internal format.
- This was an API regression from a Microsoft change.
- Filed as [onedrive-api-docs#944](https://github.com/OneDrive/onedrive-api-docs/issues/944).
- Eventually resolved by Microsoft.

**Impact:** Was critical (completely broke Business account sync). Now resolved on
Microsoft's side.

**Workaround:** Issue was fixed server-side by Microsoft.

**Relevance to Our Implementation:** LOW (resolved). However, this reinforces that we should
use the driveId exactly as returned by the API for constructing request URLs, rather than
transforming the case.

### 5.3 App_Code / App_Data Folders Return 404 (Issue #200)

**Summary:** Folders named `App_Code` or `App_Data` returned HTTP 404 when queried via the
Graph API, because these names are reserved by the underlying SharePoint/IIS infrastructure.

**Technical Details:**
- The Graph API path-based queries URL-encode the path, producing requests like
  `.../root:/App_Code`. The SharePoint backend intercepts these as IIS reserved paths and
  returns a 404 HTML page instead of JSON.
- The client crashed when trying to parse the HTML response as JSON.
- Filed as [onedrive-api-docs#914](https://github.com/OneDrive/onedrive-api-docs/issues/914).
- Microsoft deployed a fix.

**Impact:** Was high (prevented syncing certain folder names). Now resolved.

**Workaround:** Issue was fixed server-side by Microsoft.

**Relevance to Our Implementation:** LOW (resolved). Our JSON parsing should still be
defensive -- if an API response starts with `<` instead of `{`, it should be treated as an
error rather than causing a JSON parse crash.

---

## 6. Delta Query and Ordering Issues

### 6.1 Changes Applied in Wrong Order (Issue #154)

**Summary:** The delta API returns changes in an order where deletions come after
creations, causing the client to download new files and then immediately delete them.

**Technical Details:**
- Scenario: User deletes a directory remotely and recreates it with some different files.
- Delta response ordering:
  1. New folder creation
  2. New file downloads
  3. Old item deletions (including items that share names/paths with newly created items)
- The client processes changes in the order received, so it downloads new files and then
  deletes them (and the parent directory), because the deletion entries reference the OLD
  item IDs which map to the same paths.
- This causes data loss: the newly created files are deleted locally, and then the client
  uploads the deletion back to OneDrive, wiping the remote copy too.
- The underlying API behavior was confirmed: deletions and creations are interleaved in
  delta responses with no guaranteed ordering.

**Impact:** Critical -- DATA LOSS. Files are downloaded and then immediately deleted, and
the deletion propagates back to the server.

**Workaround:** The reference implementation added logic to process deletions before other
operations by sorting the delta response. However, this is inherently fragile because the
item IDs differ between old (deleted) and new (created) items even when paths overlap.

**Relevance to Our Implementation:** CRITICAL. We MUST NOT process delta changes in raw
order. Our sync algorithm must:
1. Separate deletions from other operations.
2. Process deletions first, matching by item ID (not path) to avoid deleting newly
   created items that happen to share the same path.
3. Use item IDs as the primary identity, never paths, for determining what to delete.
4. Consider a two-pass approach: first pass identifies all ID-to-path mappings, second pass
   applies operations in safe order.

### 6.2 Always Asking for Resync (Issue #3180)

**Summary:** The client enters an infinite loop of requesting `--resync` even when
`--resync` is provided in the same command.

**Technical Details:**
- After performing a resync (deleting the database and re-fetching all state), the
  application immediately detects "a cache state issue" and requests another resync.
- Root cause: the driveId inconsistency bugs (Sections 1.1, 1.2) mean that the freshly
  built database already contains inconsistencies, because the delta response itself
  contains mixed-format driveIds.
- The "cache state issue" detection runs after database rebuild and finds mismatches.
- This creates an unrecoverable loop for affected accounts.

**Impact:** Critical. The client becomes completely unusable -- it cannot even complete an
initial sync.

**Workaround:** Build from master with driveId normalization fixes.

**Relevance to Our Implementation:** HIGH. Our resync/full-rebuild logic must normalize all
IDs during the rebuild process. The consistency check must use the same normalization as the
database write path.

---

## 7. Upload and Download Issues

### 7.1 Retry Upload Always Fails (Issue #2306)

**Summary:** When a large file upload is interrupted and the client attempts to resume, the
API returns HTTP 416 ("The uploaded fragment has already been received") for every retry,
making resumption impossible.

**Technical Details:**
- Large files are uploaded in fragments using upload sessions.
- If a fragment is partially or fully received but the client does not get confirmation
  (e.g., due to network timeout), the client retries the same fragment.
- The API returns 416 (Range Not Satisfiable), indicating the fragment was already received.
- The client's retry logic does not handle this case: it does not query the upload session
  status to determine which ranges have been received, and instead blindly retries from
  the last locally tracked position.

**Impact:** Medium. Large files cannot be uploaded after any interruption, requiring manual
intervention (deleting the upload session tracking file).

**Workaround:** Delete the partial upload session tracking file and restart the upload from
scratch.

**Relevance to Our Implementation:** HIGH. Our upload session resume logic must:
1. Query the upload session status endpoint to determine which byte ranges have been
   accepted.
2. Resume from the next un-received byte range, not from the last locally tracked position.
3. Handle HTTP 416 by re-querying the session status rather than retrying the same fragment.

### 7.2 Multiple Versions After Single Upload (Issue #2)

**Summary:** On OneDrive Business, uploading a single file creates two versions, doubling
storage consumption.

**Technical Details:**
- When a file is uploaded, the client sets the file content first, then PATCHes the
  metadata (specifically `fileSystemInfo.lastModifiedDateTime`) to match the local file's
  modification time.
- SharePoint treats the metadata PATCH as a modification event and creates a second version.
- Result: every uploaded file consumes 2x its actual size in storage quota.
- Filed as [onedrive-api-docs#778](https://github.com/OneDrive/onedrive-api-docs/issues/778)
  and [onedrive-api-docs#877](https://github.com/OneDrive/onedrive-api-docs/issues/877).
- Microsoft confirmed both as API bugs.

**Impact:** Medium. Storage quota consumed at 2x rate. Only affects Business accounts.

**Workaround:** The reference implementation switched to session uploads that include
`fileSystemInfo` in the upload session creation request, setting timestamps atomically
with the file content. This resolved the double-version for new uploads. Modified file
re-uploads still create two versions due to the separate API bug
(onedrive-api-docs#877).

**Relevance to Our Implementation:** HIGH. We must use upload sessions that include
`fileSystemInfo` in the creation request. Never PATCH timestamps separately after upload.
For modified files, be aware that Business/SharePoint will still create an extra version --
this is a known, unfixed Microsoft bug.

---

## 8. SharePoint-Specific Issues

### 8.1 SharePoint Overwrote Shared File -- Data Loss (Issue #2353)

**Summary:** The sync client uploaded a local file to SharePoint, overwriting a colleague's
active edits, resulting in data loss.

**Technical Details:**
- SharePoint's upload mechanism requires deleting the existing file and re-uploading due
  to SharePoint file enrichment that prevents in-place updates
  ([onedrive-api-docs#935](https://github.com/OneDrive/onedrive-api-docs/issues/935)).
- The delete-and-reupload pattern means that if a colleague is actively editing a file via
  SharePoint Online (co-authoring), their changes may be lost.
- System file indexers (like Ubuntu's `tracker3`) can modify file timestamps without
  changing content, triggering spurious upload events.
- The deleted file should be recoverable from the SharePoint Recycle Bin, but the reporter
  stated it was not found there.

**Impact:** Critical -- DATA LOSS. Active co-authored documents can be overwritten.

**Workaround:** None that is fully reliable. Careful handling of inotify events to filter
out indexer-triggered modifications. Check for HTTP 423 (Locked) status before uploading.

**Relevance to Our Implementation:** CRITICAL. For SharePoint document libraries:
1. Always check lock status (HTTP 423) before uploading.
2. Consider using the SharePoint co-authoring endpoints rather than delete-and-reupload.
3. Filter inotify events to ignore timestamp-only modifications (no content change).
4. Implement a "safety delay" between detecting a local change and uploading, to reduce
   race conditions with co-authors.

---

## 9. Unsupported Features (Blockers)

### 9.1 Microsoft Teams Files (Issue #2358)

**Summary:** Files shared via Microsoft Teams channels are not accessible through the
`/me/drive` or shared folders API endpoints.

**Technical Details:**
- Teams files are stored in SharePoint document libraries associated with Team channels.
- The `--list-shared-folders` command does not show Teams folders.
- Accessing Teams files requires knowing the specific SharePoint site ID and document
  library ID for the Team.

**Relevance to Our Implementation:** LOW (initial release). Document the limitation. If
needed later, support can be added via SharePoint site enumeration APIs.

### 9.2 Personal Vault (Issue #2053)

**Summary:** The OneDrive Personal Vault feature is not accessible via the Graph API.

**Technical Details:**
- Personal Vault is a protected folder within OneDrive Personal that requires additional
  authentication.
- The Graph API does not expose any endpoints for accessing or managing the Personal Vault.
- Microsoft has not indicated plans to add API support.

**Relevance to Our Implementation:** LOW. Document as an unsupported feature. No API
access means no possible implementation.

### 9.3 SharePoint Shortcuts (Issue #1224)

**Summary:** "Add Shortcut to My Files" creates links in OneDrive that point to SharePoint
document libraries, but the Graph API does not expose these shortcuts.

**Relevance to Our Implementation:** MEDIUM. See Section 3.3 above.

---

## 10. Critical Design Implications

The issues cataloged above reveal several systemic problems with the Microsoft Graph API
that directly inform our sync engine design. Below are the critical takeaways, ordered by
priority.

### 10.1 DriveId Normalization is Non-Negotiable

The driveId truncation bug (#3072, #3115, #3336) and casing inconsistency (#3336) are the
single most impactful class of API bugs. They affect Personal accounts with driveIds
starting with zero and manifest across multiple API endpoints. **Every driveId received from
any API response must be normalized before any other processing:**

- Left-pad with zeros to exactly 16 hex characters.
- Convert to a canonical case (recommend lowercase).
- This normalization must happen at the API client layer, in the JSON response parser,
  before any value is returned to the sync engine.

### 10.2 Delta Ordering Cannot Be Trusted

The delta API does not guarantee a safe processing order (Issue #154). Deletions may appear
after creations for items at the same path. **Our sync algorithm must:**

- Parse the entire delta response before taking any action.
- Separate operations by type (deletes, creates/modifies).
- Match deletions by item ID, never by path.
- Apply deletions to the local state model first, then apply creations and modifications.
- Never propagate a deletion back to the server if the item ID no longer exists remotely.

### 10.3 Hashes Are Unreliable -- Plan for Fallbacks

The API provides no hash for: zero-byte files, some Business/SharePoint files, deleted
items, and folders on Business accounts. **Our file comparison logic must implement a
fallback chain:**

1. QuickXorHash (preferred, works on both Personal and Business).
2. SHA256Hash (available on some Business accounts).
3. Size + eTag + lastModifiedDateTime (when no hash is available).
4. For zero-byte files, comparison by size alone is sufficient.

### 10.4 Shared Folders Are a Minefield

Shared folders combine the driveId bugs with cross-drive references, making them the most
fragile part of the sync surface. Our implementation should:

- Treat shared folders as isolated sync roots with their own driveId context.
- Never assume parentReference driveIds match the shared folder's actual driveId.
- Handle path computation failures gracefully (skip the item and log) rather than crashing.
- Support configurable exclusion of specific shared folders.

### 10.5 Upload Sessions Must Be Atomic

To avoid double-versioning on Business/SharePoint (#2) and to support reliable resume
after interruption (#2306):

- Always use upload sessions (not simple PUT) for files larger than a threshold.
- Include `fileSystemInfo` (timestamps) in the upload session creation request.
- Never PATCH timestamps separately after upload.
- On resume, query the session status endpoint to determine accepted ranges before
  retrying.

### 10.6 Defensive JSON Parsing

Multiple issues involve unexpected API responses:
- HTML instead of JSON (Issue #200 -- App_Code/App_Data).
- HTTP 308 without Location header (Issue #3167).
- Missing expected fields like `file`, `folder`, or hash facets (Issue #2938).

**Our JSON parsing must:**
- Validate response content-type before parsing.
- Treat missing fields as expected variations, not fatal errors.
- Log unexpected responses at debug level with full response details for troubleshooting.
- Never crash on malformed or unexpected API responses.

### 10.7 SharePoint Upload Requires Special Care

SharePoint's file enrichment mechanism means that simple PUT uploads may not work correctly.
The delete-and-reupload pattern used by the reference implementation is dangerous for
co-authored files. **For SharePoint support, we must:**

- Check file lock status before any upload operation.
- Use upload sessions with conflict handling.
- Implement a delay between local change detection and upload to reduce co-authoring
  conflicts.
- Clearly document the data loss risk with SharePoint co-authoring.

### 10.8 Database Schema Must Be Resilient

The reference implementation's strict database constraints (NOT NULL, FOREIGN KEY) amplified
API inconsistencies into fatal crashes. **Our database design should:**

- Use nullable fields where the API may omit data (e.g., type for package items, hash
  values).
- Implement deferred foreign key checking or handle constraint violations gracefully.
- Normalize all identifiers before storage.
- Support "partial state" -- an item can exist in the database even if its parent is not
  yet known, with reconciliation happening in a subsequent pass.
