# Graph API Quirks

Known bugs, inconsistencies, and undocumented behaviors in the Microsoft Graph API discovered through this project and the abraunegg/onedrive community. These are observations that cannot be found in official documentation.

## DriveId Inconsistencies

### DriveId Truncation (Personal Accounts)

Personal account driveIds are 16 hexadecimal characters. The API intermittently drops leading zeros, returning 15-character values. Example: `024470056f5c3e43` (correct) → `24470056f5c3e43` (truncated). This occurs in delta responses, shared folder parent references, and folder creation responses. The bug is documented in [onedrive-api-docs#1890](https://github.com/OneDrive/onedrive-api-docs/issues/1890) but remains unfixed.

Without normalization, this causes FOREIGN KEY constraint failures, assertion crashes, and database consistency errors that cascade into infinite resync loops.

### DriveId Casing Inconsistency

The same driveId is returned as uppercase in some contexts and lowercase in others (e.g., `d8ebbd13a0551216` vs `D8EBBD13A0551216`). This occurs across delta responses, folder creation responses, and item ID prefixes. Affects primarily Personal accounts but can occur on any account type.

### Normalization Requirement

Every driveId from any API response must be lowercased and zero-padded to 16 characters before storage or comparison. This is enforced by the `driveid.ID` type at construction time.

## Hash and Content Integrity

### Bogus Hashes on Deleted Items

Delta responses include QuickXorHash, SHA1, or SHA256 hashes on items marked as deleted. These hashes are stale (from the item's last live state) or entirely bogus. The normalization pipeline clears all hashes on deleted items.

### Files Without Any Hash

Some Business/SharePoint files have no hash values at all. Microsoft confirmed this is a known issue: for enumeration/delta scenarios, hash generation may be "too expensive" for certain files. Zero-byte files consistently have no hash. Fallback: size + mtime + eTag comparison.

### iOS HEIC File Metadata Mismatch

`.heic` files uploaded from iOS via the OneDrive app have metadata (size, hash) that doesn't match the actual download content. The API reports the original file size but serves a post-processed (smaller) version. Example: API reports 3,039,814 bytes, actual download is 682,474 bytes. Filed as [onedrive-api-docs#1723](https://github.com/OneDrive/onedrive-api-docs/issues/1723), unresolved.

### SharePoint File Enrichment

SharePoint injects metadata into uploaded files, changing the hash and size post-upload. This can cause infinite upload/download loops if upload validation compares against the original local file. Filed as [onedrive-api-docs#935](https://github.com/OneDrive/onedrive-api-docs/issues/935).

### Upload Creates Double Versions (Business/SharePoint)

On Business/SharePoint, uploading a file and then PATCHing its `fileSystemInfo` (to preserve local timestamps) creates two versions, doubling storage consumption. The fix is to include `fileSystemInfo` in the upload session creation request, setting timestamps atomically with the content. Modified file re-uploads still create an extra version (unfixed API bug, [onedrive-api-docs#877](https://github.com/OneDrive/onedrive-api-docs/issues/877)).

### Simple Upload Cannot Set Metadata

Simple upload (PUT `/content`) sends raw binary — there's no way to include `fileSystemInfo` in the same request. Files ≤4 MiB uploaded via simple upload get server receipt timestamps. A post-upload PATCH to `UpdateFileSystemInfo` is required when mtime preservation matters.

## Delta Response Quirks

### Deletion/Creation Ordering

When a file is renamed and a new file is created with the old name, the delta response may list the creation before the deletion. Processing in order causes 409 Conflict errors. The normalization pipeline reorders deletions before creations within each parent ID group.

### Duplicate Items in Single Batch

The same item can appear multiple times in a single delta response page or across pages. The normalization pipeline deduplicates by keeping only the last occurrence of each item ID.

### URL-Encoded Item Names

Some responses return item names with URL encoding (e.g., `%20` for spaces, `%E6%97%A5%E6%9C%AC%E8%AA%9E.txt` for `日本語.txt`). This is inconsistent — most responses return plain UTF-8. The normalization pipeline decodes all item names unconditionally.

### OneNote Package Items

Items with a `package` facet (`type: "oneNote"`) have no `file` or `folder` facet. Database schemas expecting every item to have a type will fail. The normalization pipeline filters package items.

### Delta Endpoint Consistency Lag

The delta endpoint (`/drives/{driveID}/root/delta`) aggregates changes from a different consistency domain than REST item endpoints. After a mutation (e.g., `DELETE /items/{id}`), a direct `GET` on the item returns 404 within seconds, but the delta endpoint may not include the change for 5-60+ seconds. This causes the delta response to lag behind the observable state of individual items.

Microsoft acknowledges this: "Due to replication delays, changes to the object do not show up immediately [...] You should retry [...] after some time to retrieve the latest changes."

### Ephemeral Deletion Events

Delta deletion events are delivered **exactly once**. If the client's token window advances past a deletion (including via a zero-event response that returns a new token), the deletion is permanently missed. A subsequent incremental delta call will never report that deletion again. Fresh delta (no token) enumerates only existing items — it never reports deletions. This means incremental delta is the **only** way to learn about deletions, and it has exactly one chance to deliver each one.

The combination of consistency lag (above) and ephemeral deletions creates a window where: (1) a deletion is performed, (2) the client calls delta before the deletion propagates to the change log, (3) delta returns zero events + new token, (4) the client saves the new token, permanently skipping the deletion.

Mitigation requires both a zero-event token guard (don't advance token on empty responses) and periodic full reconciliation (enumerate all items, detect orphans in baseline).

### Personal Phantom System Drives

Every Personal account has 2-3 hidden system drives (face crops, albums) created by Microsoft for the Photos app. They report `driveType: "personal"` and share quota numbers with the real OneDrive, but return HTTP 400 `ObjectHandle is Invalid` when accessed. `GET /me/drive` (singular) returns the real drive. `GET /me/drives` (plural) returns all drives in non-deterministic order.

## HTTP Protocol Violations

### Transient 404 on Valid Resources

`GET /drives/{driveID}/items/root/children` returns HTTP 404 ("itemNotFound") on drives that have existed for months. The token is valid, the drive ID is correct, and subsequent requests succeed. Caused by cross-datacenter lookup timeouts on the load balancer. This is a server-side failure misreported as a client error. Frequency: ~1% of requests.

### Transient 403 on `/me/drives` (Token Propagation)

After token refresh, `/me/drives` returns HTTP 403 ("accessDenied") while `/me` (user profile) succeeds with the same token. Caused by eventual consistency in Microsoft's token propagation infrastructure. The `/me` endpoint receives token updates before `/me/drives`.

### HTTP 308 Without Location Header

Webhook subscription renewal (PATCH to `/v1.0/subscriptions/{id}`) returns HTTP 308 (Permanent Redirect) without a `Location` header, violating RFC 7538. Observed on Personal accounts migrated to the new backend, specifically in the West Europe data center. Filed as [onedrive-api-docs#1895](https://github.com/OneDrive/onedrive-api-docs/issues/1895).

### HTTP 400 "ObjectHandle is Invalid" (Outage Pattern)

Multi-hour Microsoft-side backend failures return HTTP 400 `invalidRequest` / `"ObjectHandle is Invalid"` on every Graph API call. Not endpoint-specific — affects all operations. Non-retryable (400 is a client error code). The only resolution is to wait for Microsoft to fix it. Observed: one 3.5-hour incident.

### HTTP 507 Wraps ErrServerError

HTTP 507 (Insufficient Storage) wraps `ErrServerError` at the sentinel level. If error classification checks sentinels before status codes, 507 is incorrectly classified as retryable (like other 5xx errors). Status code classification must take priority.

## Throttling Scopes

Microsoft Graph API throttling scope behavior (not in official docs, discovered through testing and community):

- **Per-user limit**: All requests from the same user (same OAuth token) share a throttle bucket, regardless of which drive or folder is targeted.
- **Per-tenant limit**: All users in an organization share a higher aggregate limit.
- **Per-app-per-tenant limit**: Each registered app has its own per-tenant quota.
- **Implication for sync**: HTTP 429 on any drive (including shortcuts) means the entire account is throttled. Shortcut drives that belong to different users still share the caller's rate limit (the caller's token is used, not the sharer's).
- `Retry-After` on 429/503 provides server-mandated wait time. `RateLimit-Remaining` and `RateLimit-Limit` headers exist but only cover per-app 1-minute resource units — not reliable enough for proactive throttling.

## Shared Folder Issues

### SharedWithMe Identity Response Shape

`/me/drive/sharedWithMe` returns identity data under `remoteItem.shared` and `remoteItem.createdBy`, NOT top-level `shared`. Four-level fallback chain: `remoteItem.shared.sharedBy` → `.owner` → `remoteItem.createdBy` → top-level `shared.owner`. The `email` field in identity responses is undocumented but works on both personal and business accounts.

### SharedWithMe API Deprecation

`/me/drive/sharedWithMe` and `/me/drive/recent` are deprecated (November 2026 EOL). Non-deprecated alternative: `GET /me/drive/search(q='*')` returns shared items with `remoteItem` facet but less identity data (no email). Enrich via `GET /drives/{driveId}/items/{itemId}`. `/me/drives` is NOT deprecated.

### Cross-Organization Shared Folders Invisible (Business)

Shared folders from users outside your organization are not visible via the Graph API, even though they appear in the OneDrive web interface. This is a fundamental Graph API limitation, not a client bug.

### Folder-Scoped Delta Limitation

On OneDrive for Business and SharePoint, the delta action is only available on the `root` item of a drive. Folder-scoped delta (`/drives/{driveID}/items/{folderID}/delta`) only works on OneDrive Personal. This is a documented API limitation, not a bug.

### Personal Shared Folders — Inconsistent Parent References

Shared folder items in delta responses reference parent IDs on a different drive (the sharer's drive). The driveId in these references is subject to the truncation and casing bugs. URL-encoded spaces appear in `parentReference.path` ([onedrive-api-docs#1765](https://github.com/OneDrive/onedrive-api-docs/issues/1765)).

## Upload Session Quirks

### Upload Resume Returns 416

When a fragment is partially received but the client doesn't get confirmation (network timeout), retrying the same fragment returns HTTP 416 ("Range Not Satisfiable"). The client must query the session status endpoint to discover `nextExpectedRanges` before retrying.

### eTag Changes During Upload Session

Creating an upload session with `If-Match: {eTag}` can cause the eTag to change during session creation, leading to 412 Precondition Failed followed by 416 on subsequent fragments. Do not use `If-Match` on upload session creation.

### Upload Session Leak

`CreateUploadSession` succeeds but subsequent operations fail. The session persists server-side until expiry (~15 minutes) and counts against quotas. Always cancel sessions on failure using `context.Background()` (the original context may be canceled).

## Timestamp and Metadata

### OneDrive Has Whole-Second Precision

OneDrive does not store fractional seconds. Timestamps must be truncated to zero fractional seconds before comparison.

### Invalid Timestamps

The API can return `0001-01-01T00:00:00Z` or far-future dates. `lastModifiedDateTime` may be absent for API-initiated deletions.

### URL-Encoded Paths

`parentReference.path` (when present in non-delta responses) contains URL-encoded characters (`%20` for spaces). Must be decoded before use.

## Case Sensitivity

### OneDrive Is Case-Insensitive

Two files differing only in case cannot coexist in the same OneDrive folder. The API's path-based queries perform case-insensitive matching but can return items from incorrect paths (observed with git repository files where `v1.0.0` and `v2.0.0` produce false matches).

## Normalization Pipeline

All delta responses pass through a 5-stage normalization pipeline (`internal/graph/normalize.go`) before the sync engine processes them:

1. Decode URL-encoded names
2. Filter OneNote packages
3. Clear deleted item hashes
4. Deduplicate (keep last occurrence per item ID)
5. Reorder deletions before creations (within each parent)

## Recycle Bin API — Personal vs Business

The `/drives/{id}/special/recyclebin/children` endpoint only works on Business/SharePoint drives. Personal OneDrive accounts return **HTTP 400** (`"The special folder identifier isn't valid"`) because the `special` folder collection does not include `recyclebin` on Personal accounts.

Similarly, `permanentDelete` (used by `recycle-bin empty`) returns **HTTP 405** on Personal accounts — the code already handles this by falling back to regular `DELETE` (`recycle_bin.go`).

**Impact**: `recycle-bin list` and `recycle-bin restore` are Business-only. E2E tests skip on Personal accounts.
