# Microsoft Graph API DriveItem Field Matrix

This document provides a comprehensive inventory of every field available on the Microsoft Graph API DriveItem resource type, along with a detailed availability matrix showing which fields are present across different contexts, account types, and API endpoints.

**Sources:** Official Microsoft Graph API documentation (v1.0), abraunegg/onedrive issue tracker, Tier 1 research documents (api-analysis.md, issues-graph-api-bugs.md, issues-api-inconsistencies.md, ref-edge-cases.md).

---

## 1. DriveItem Fields Inventory

DriveItem inherits from baseItem and adds numerous facets. Below is the complete inventory grouped by category.

### 1.1 Core Identity Fields (inherited from baseItem)

| Field | Type | Description | Read/Write |
|-------|------|-------------|------------|
| `id` | String | Unique identifier of the item within the drive | Read-only |
| `name` | String | Display name (filename + extension) | Read-write |
| `eTag` | String | ETag for the entire item (metadata + content) | Read-only |
| `webUrl` | String | URL to view the resource in a browser | Read-only |

### 1.2 Timestamp Fields

| Field | Type | Description | Read/Write |
|-------|------|-------------|------------|
| `createdDateTime` | DateTimeOffset | Server-side creation timestamp | Read-only |
| `lastModifiedDateTime` | DateTimeOffset | Server-side last modification timestamp | Read-only |
| `createdBy` | identitySet | Identity of creator (user, device, application) | Read-only |
| `lastModifiedBy` | identitySet | Identity of last modifier (user, device, application) | Read-only |

### 1.3 Content Metadata Fields

| Field | Type | Description | Read/Write |
|-------|------|-------------|------------|
| `cTag` | String | Content tag; changes only when file content changes | Read-only |
| `size` | Int64 | Size of the item in bytes | Read-only |
| `description` | String | User-visible description (Personal only) | Read-write |
| `webDavUrl` | String | WebDAV-compatible URL for the item | Read-only |

### 1.4 Parent Reference Fields (itemReference)

The `parentReference` field is an itemReference object with the following sub-fields:

| Field | Type | Description |
|-------|------|-------------|
| `parentReference.id` | String | ID of the parent folder |
| `parentReference.driveId` | String | ID of the drive containing the parent |
| `parentReference.driveType` | String | Type of drive: `personal`, `business`, `documentLibrary` |
| `parentReference.path` | String | Percent-encoded path relative to drive root |
| `parentReference.name` | String | Name of the parent item |
| `parentReference.shareId` | String | Unique identifier for shared resources |
| `parentReference.sharepointIds` | sharepointIds | SharePoint REST compatibility IDs |
| `parentReference.siteId` | String | Site ID (Business/SharePoint only) |

### 1.5 Facets

Facets are optional complex objects whose presence indicates the item's type or capabilities.

#### File Facet

| Field | Type | Description |
|-------|------|-------------|
| `file` | file | Present when item is a file |
| `file.mimeType` | String | MIME type (determined server-side) |
| `file.hashes` | hashes | Hash values for the file content |

#### Hash Fields (within file.hashes)

| Field | Type | Encoding | Description |
|-------|------|----------|-------------|
| `file.hashes.quickXorHash` | String | Base64 | Proprietary hash; only hash guaranteed on all account types |
| `file.hashes.sha1Hash` | String | Hex | SHA-1 hash (Personal accounts only) |
| `file.hashes.sha256Hash` | String | Hex | SHA-256 hash (officially unsupported per docs, but seen on Business) |
| `file.hashes.crc32Hash` | String | Hex | CRC32 hash (sometimes on Personal) |

#### Folder Facet

| Field | Type | Description |
|-------|------|-------------|
| `folder` | folder | Present when item is a folder |
| `folder.childCount` | Int32 | Number of immediate children |
| `folder.view` | folderView | Recommended view properties |

#### Other Type Facets

| Field | Type | Description |
|-------|------|-------------|
| `deleted` | deleted | Present when item has been deleted |
| `deleted.state` | String | State of the deleted item |
| `root` | root | Present (non-null, empty object) when item is the drive root |
| `package` | package | Present for package items (e.g., OneNote notebooks) |
| `package.type` | String | Package type identifier (e.g., `"oneNote"`) |
| `remoteItem` | remoteItem | Present when item references an item on another drive |
| `shared` | shared | Present when item has been shared with others |
| `specialFolder` | specialFolder | Present for well-known special folders |
| `specialFolder.name` | String | Folder identifier in `/drive/special` collection |
| `searchResult` | searchResult | Present when item comes from a search result |

#### Media Facets

| Field | Type | Description |
|-------|------|-------------|
| `audio` | audio | Audio metadata (Personal only) |
| `image` | image | Image metadata |
| `photo` | photo | Photo metadata (camera, date taken, device) |
| `video` | video | Video metadata |
| `location` | geoCoordinates | Geographic coordinates |

#### FileSystemInfo Facet

| Field | Type | Description |
|-------|------|-------------|
| `fileSystemInfo` | fileSystemInfo | Client-reported filesystem timestamps |
| `fileSystemInfo.createdDateTime` | DateTimeOffset | Client-reported UTC creation time |
| `fileSystemInfo.lastModifiedDateTime` | DateTimeOffset | Client-reported UTC last modification time |
| `fileSystemInfo.lastAccessedDateTime` | DateTimeOffset | Last access time (only in recent file list) |

#### Other Facets

| Field | Type | Description |
|-------|------|-------------|
| `malware` | malware | Present if item was flagged as malware |
| `pendingOperations` | pendingOperations | Present if operations are pending completion |
| `publication` | publicationFacet | Published/checked-out state (not returned by default) |
| `bundle` | bundle | Bundle metadata |

### 1.6 SharePoint Compatibility Fields

| Field | Type | Description |
|-------|------|-------------|
| `sharepointIds` | sharepointIds | SharePoint REST compatibility identifiers |
| `sharepointIds.listId` | String | GUID of the SharePoint list |
| `sharepointIds.listItemId` | String | Integer ID within the list |
| `sharepointIds.listItemUniqueId` | String | GUID for the list item |
| `sharepointIds.siteId` | String | GUID of the site collection |
| `sharepointIds.siteUrl` | String | SharePoint URL for the site |
| `sharepointIds.tenantId` | String | GUID of the tenant |
| `sharepointIds.webId` | String | GUID of the site (SPWeb) |

**Note:** `sharepointIds` is never populated for OneDrive Personal items.

### 1.7 RemoteItem Fields

When a DriveItem has a non-null `remoteItem` facet, it contains a mirror of most DriveItem properties for the referenced item on another drive:

| Field | Type | Description |
|-------|------|-------------|
| `remoteItem.id` | String | ID on the remote drive |
| `remoteItem.name` | String | Name of the remote item |
| `remoteItem.size` | Int64 | Size in bytes |
| `remoteItem.createdBy` | identitySet | Creator identity |
| `remoteItem.createdDateTime` | Timestamp | Creation time |
| `remoteItem.lastModifiedBy` | identitySet | Last modifier identity |
| `remoteItem.lastModifiedDateTime` | Timestamp | Last modification time |
| `remoteItem.file` | file | File facet (with hashes) |
| `remoteItem.folder` | folder | Folder facet |
| `remoteItem.fileSystemInfo` | fileSystemInfo | Client-reported timestamps (may be null) |
| `remoteItem.image` | image | Image metadata |
| `remoteItem.video` | video | Video metadata |
| `remoteItem.package` | package | Package metadata |
| `remoteItem.parentReference` | itemReference | Parent on the remote drive |
| `remoteItem.shared` | shared | Sharing information |
| `remoteItem.sharepointIds` | sharepointIds | SharePoint IDs |
| `remoteItem.specialFolder` | specialFolder | Special folder designation |
| `remoteItem.webDavUrl` | String | WebDAV URL |
| `remoteItem.webUrl` | String | Browser URL |

### 1.8 Instance Attributes (Annotations)

| Field | Type | Description | Read/Write |
|-------|------|-------------|------------|
| `@microsoft.graph.downloadUrl` | String | Pre-authenticated download URL (expires ~1 hour) | Read-only |
| `@microsoft.graph.conflictBehavior` | String | Conflict resolution: `fail`, `replace`, `rename` | Write-only |
| `@microsoft.graph.sourceUrl` | String | Source URL for server-side download+store | Write-only |

---

## 2. Field Availability Matrix

### 2.1 Core Fields by API Context

| Field | Delta Response | Direct GET | /children | Search Results |
|-------|:-------------:|:----------:|:---------:|:--------------:|
| `id` | Always | Always | Always | Always |
| `name` | Usually (see 2.3) | Always | Always | Always |
| `eTag` | Usually (see 2.4) | Usually (see 2.4) | Always | Always |
| `cTag` | Sometimes (see 2.5) | Sometimes (see 2.5) | Sometimes | Sometimes |
| `size` | Usually (see 2.3) | Always | Always | Always |
| `createdDateTime` | Always | Always | Always | Always |
| `lastModifiedDateTime` | Usually | Always | Always | Always |
| `createdBy` | Always | Always | Always | Always |
| `lastModifiedBy` | Always | Always | Always | Always |
| `webUrl` | Always | Always | Always | Always |
| `webDavUrl` | Sometimes | Sometimes | Sometimes | Sometimes |
| `parentReference` | Always (partial) | Always | Always | Always |
| `parentReference.id` | Always | Always | Always | Always |
| `parentReference.driveId` | Always | Always | Always | Always |
| `parentReference.driveType` | Always | Always | Always | Always |
| `parentReference.path` | **Never** | Always | Always | Always |
| `parentReference.name` | Sometimes | Sometimes | Sometimes | Sometimes |
| `fileSystemInfo` | Usually | Always | Always | Usually |
| `file` | For files | For files | For files | For files |
| `file.hashes` | For files | For files | For files | For files |
| `folder` | For folders | For folders | For folders | For folders |
| `deleted` | For deleted items | N/A | N/A | N/A |
| `root` | For root item | For root item | N/A | N/A |
| `remoteItem` | For shared items | For shared items | For shared items | For shared items |
| `package` | For packages | For packages | For packages | For packages |
| `shared` | For shared items | For shared items | For shared items | For shared items |
| `specialFolder` | For specials | For specials | For specials | For specials |
| `@microsoft.graph.downloadUrl` | Sometimes | Always (for files) | Sometimes | Sometimes |
| `sharepointIds` | Business/SP only | Business/SP only | Business/SP only | Business/SP only |

### 2.2 Hash Availability by Account Type

| Hash Type | JSON Path | Personal | Business | SharePoint |
|-----------|-----------|:--------:|:--------:|:----------:|
| QuickXorHash | `file.hashes.quickXorHash` | Always | Always | Always |
| SHA-1 | `file.hashes.sha1Hash` | Always | Never | Never |
| SHA-256 | `file.hashes.sha256Hash` | Never | Sometimes | Sometimes |
| CRC32 | `file.hashes.crc32Hash` | Sometimes | Never | Never |

**Notes:**
- QuickXorHash is the **only** hash guaranteed across all account types. It uses Base64 encoding.
- SHA-256 is listed as "not supported" in official docs but has been observed on some Business accounts.
- SHA-1 and CRC32 use hex encoding.
- Zero-byte files may have no hash, or a known constant QuickXorHash of `AAAAAAAAAAAAAAAAAAAAAAAAAAA=` (all zeros).
- Some Business/SharePoint files (particularly in delta/enumeration scenarios) may lack all hashes; Microsoft has acknowledged this as a known issue for files where hash generation is "too expensive."

### 2.3 Fields Omitted for Deleted Items (Delta Responses)

| Field | Personal (Deleted) | Business (Deleted) | SharePoint (Deleted) |
|-------|:------------------:|:------------------:|:--------------------:|
| `id` | Present | Present | Present |
| `name` | Present | **Omitted** | **Omitted** |
| `size` | **Omitted** | Present | Present |
| `cTag` | Omitted | Omitted | Omitted |
| `eTag` | Present | Present | Present |
| `file` / `folder` | Present | Present | Present |
| `file.hashes` | **Bogus values** | **Bogus values** | **Bogus values** |
| `deleted` | Present | Present | Present |
| `parentReference.id` | Present | Present | Present |
| `parentReference.driveId` | Present | Present | Present |
| `parentReference.path` | Never | Never | Never |
| `lastModifiedDateTime` | Usually | Usually | Usually |
| `fileSystemInfo` | Sometimes | Sometimes | Sometimes |

**Key insight:** On Business accounts, the only reliable identifier for a deleted item is `id`. The `name` may be absent, forcing the sync engine to look up names from its local database.

### 2.4 eTag Availability

| Context | Personal | Business | SharePoint |
|---------|:--------:|:--------:|:----------:|
| Regular file | Always | Always | Always |
| Regular folder | Always | Always | Always |
| Root item | Always | **Omitted** | **Omitted** |
| Deleted item | Present | Present | Present |
| Delta response | Present | Present (except root) | Present (except root) |
| Direct GET | Present | Present (except root) | Present (except root) |

**Key insight:** The root item of OneDrive for Business and SharePoint drives does not return an eTag. Do not depend on eTag for concurrency control on root items.

### 2.5 cTag Availability

| Context | Personal | Business | SharePoint |
|---------|:--------:|:--------:|:----------:|
| File (direct GET) | Always | Always | Always |
| File (delta create/modify) | Always | **Omitted** | **Omitted** |
| File (delta delete) | Omitted | Omitted | Omitted |
| Folder (any context) | Always | **Never** | **Never** |
| Root item | Always | **Never** | **Never** |

**Key insight:** cTag cannot be used for change detection on Business/SharePoint accounts in delta responses, because it is omitted for create/modify operations. It is also never present on Business/SharePoint folders.

### 2.6 fileSystemInfo Availability

| Context | Personal | Business | SharePoint |
|---------|:--------:|:--------:|:----------:|
| Regular file | Always | Always | Always |
| Regular folder | Always | Always | Always |
| Root item | Always | Always | Always |
| Deleted item | Sometimes | Sometimes | Sometimes |
| Remote item (remoteItem facet) | Usually | Usually | **Sometimes missing** |
| "Add Shortcut" items | Usually | **Sometimes missing** | **Sometimes missing** |

**Key insight:** Remote items created via "Add Shortcut to My Files" may lack `fileSystemInfo` in the `remoteItem` facet. Fall back to the top-level `createdDateTime`/`lastModifiedDateTime` or to `remoteItem.createdDateTime`/`remoteItem.lastModifiedDateTime`.

### 2.7 sharepointIds Availability

| Context | Personal | Business | SharePoint |
|---------|:--------:|:--------:|:----------:|
| Any item | **Never** | Always | Always |

### 2.8 parentReference.path Availability

| Context | Personal | Business | SharePoint |
|---------|:--------:|:--------:|:----------:|
| Delta response | **Never** | **Never** | **Never** |
| Direct GET | Always | Always | Always |
| /children | Always | Always | Always |
| Search results | Always | Always | Always |

This is documented behavior: delta responses never include `parentReference.path`. The sync engine must track items by `id` and reconstruct paths from the parent chain.

### 2.9 Complete Field Matrix -- Files vs Folders vs Root vs Deleted

| Field | File | Folder | Root | Deleted File | Deleted Folder |
|-------|:----:|:------:|:----:|:------------:|:--------------:|
| `id` | Yes | Yes | Yes | Yes | Yes |
| `name` | Yes | Yes | Yes | Bus: No, Per: Yes | Bus: No, Per: Yes |
| `size` | Yes | Yes (0) | Yes (0) | Per: No, Bus: Yes | Per: No, Bus: Yes |
| `eTag` | Yes | Yes | Bus/SP: No | Yes | Yes |
| `cTag` | Per: Yes, Bus: Delta No | Per: Yes, Bus: No | Per: Yes, Bus: No | No | No |
| `file` | Yes | No | No | Yes | No |
| `file.hashes` | Yes | N/A | N/A | Bogus | N/A |
| `folder` | No | Yes | Yes | No | Yes |
| `folder.childCount` | N/A | Yes | Yes | N/A | Sometimes |
| `deleted` | No | No | No | Yes | Yes |
| `root` | No | No | Yes | No | No |
| `parentReference` | Yes | Yes | Minimal | Yes | Yes |
| `fileSystemInfo` | Yes | Yes | Yes | Sometimes | Sometimes |
| `createdDateTime` | Yes | Yes | Yes | Yes | Yes |
| `lastModifiedDateTime` | Yes | Yes | Yes | Sometimes | Sometimes |
| `createdBy` | Yes | Yes | Yes | Sometimes | Sometimes |
| `lastModifiedBy` | Yes | Yes | Yes | Sometimes | Sometimes |
| `@microsoft.graph.downloadUrl` | Yes | No | No | No | No |

---

## 3. Field Reliability Issues

### 3.1 driveId Truncation (Personal Accounts)

| Aspect | Detail |
|--------|--------|
| **Affected account type** | OneDrive Personal |
| **Field** | `parentReference.driveId` |
| **Bug** | DriveIds starting with `0` are truncated from 16 to 15 characters |
| **Example** | `024470056f5c3e43` (correct) vs `24470056f5c3e43` (truncated) |
| **Contexts affected** | Delta responses, shared folder parent references |
| **Contexts unaffected** | SharePoint URL IDs, root item queries |
| **Microsoft bug** | [onedrive-api-docs#1890](https://github.com/OneDrive/onedrive-api-docs/issues/1890) |
| **Normalization** | Left-pad all Personal driveIds with zeros to exactly 16 characters |

### 3.2 driveId Casing Inconsistency

| Aspect | Detail |
|--------|--------|
| **Affected account type** | OneDrive Personal (primarily) |
| **Fields** | `parentReference.driveId`, item ID prefixes |
| **Bug** | Same driveId returned as uppercase in some contexts, lowercase in others |
| **Example** | `d8ebbd13a0551216` vs `D8EBBD13A0551216` |
| **Normalization** | Convert all driveIds to lowercase before storage or comparison |

### 3.3 Invalid/Missing Timestamps

| Issue | Detail |
|-------|--------|
| **Invalid dates** | API can return `0001-01-01T00:00:00Z` or far-future dates |
| **Missing timestamps** | `lastModifiedDateTime` may be absent for API-initiated deletions |
| **Fractional seconds** | OneDrive does not store fractional seconds; truncate to zero before comparison |
| **Mitigation** | Validate all timestamps; fall back to current UTC time if invalid or missing |

### 3.4 Bogus Hashes on Deleted Items

| Issue | Detail |
|-------|--------|
| **Affected context** | Deleted items in delta responses |
| **Symptom** | QuickXorHash may be `AAAAAAAAAAAAAAAAAAAAAAAAAAA=` (all zeros) |
| **Impact** | Hash comparison useless for confirming file identity |
| **Mitigation** | Ignore server-provided hashes for deleted items; compare against locally stored hash |

### 3.5 Missing Name on Business Deleted Items

| Issue | Detail |
|-------|--------|
| **Affected account type** | OneDrive for Business, SharePoint |
| **Context** | Delta query deleted items |
| **Impact** | Cannot determine what was deleted by name; must use `id` for lookup |
| **Mitigation** | Always look up item details from local database when processing deletions |

### 3.6 Missing Size on Personal Deleted Items

| Issue | Detail |
|-------|--------|
| **Affected account type** | OneDrive Personal |
| **Context** | Delta query deleted items |
| **Impact** | Cannot determine file size of deleted items |
| **Mitigation** | Use locally stored size from the sync database |

### 3.7 URL-Encoded Paths

| Issue | Detail |
|-------|--------|
| **Affected field** | `parentReference.path` (when present in non-delta responses) |
| **Symptom** | Spaces encoded as `%20`, other characters URL-encoded |
| **Impact** | Path matching and filter evaluation fail if not decoded |
| **Microsoft bug** | [onedrive-api-docs#1765](https://github.com/OneDrive/onedrive-api-docs/issues/1765) |
| **Mitigation** | URL-decode all `parentReference.path` values before use |

### 3.8 iOS HEIC File Metadata Mismatch

| Issue | Detail |
|-------|--------|
| **Affected files** | `.heic` files uploaded from iOS devices |
| **Symptom** | API reports original file size/hash but serves a converted (smaller) file |
| **Impact** | Download validation fails; actual data loss if validation deletes the file |
| **Microsoft bug** | [onedrive-api-docs#1723](https://github.com/OneDrive/onedrive-api-docs/issues/1723) |
| **Mitigation** | Configurable download validation; do not delete files on validation failure |

### 3.9 SharePoint File Enrichment

| Issue | Detail |
|-------|--------|
| **Affected libraries** | SharePoint document libraries |
| **Symptom** | SharePoint injects metadata into uploaded files, changing hash and size |
| **Impact** | Upload validation fails; can cause infinite upload/download loops |
| **Microsoft bug** | [onedrive-api-docs#935](https://github.com/OneDrive/onedrive-api-docs/issues/935) |
| **Mitigation** | Disable upload validation for SharePoint; use `cTag` or `eTag` change detection |

### 3.10 Summary of Field Reliability

| Field | Reliable for Sync? | Notes |
|-------|:------------------:|-------|
| `id` | **Yes** | Always present, always unique within a drive |
| `name` | Mostly | Missing on Business deleted items |
| `size` | Mostly | Missing on Personal deleted items; may be wrong for SharePoint |
| `eTag` | Mostly | Missing on Business/SharePoint root items |
| `cTag` | **No** (Business) | Omitted in delta create/modify on Business; never on Business folders |
| `quickXorHash` | **Yes** (files) | Only hash on all account types; bogus on deleted items |
| `sha1Hash` | Personal only | Never on Business/SharePoint |
| `sha256Hash` | Unreliable | Officially "not supported"; sometimes on Business |
| `parentReference.id` | **Yes** | Always present |
| `parentReference.driveId` | Yes (with normalization) | Must normalize case and length |
| `parentReference.path` | **Never in delta** | Must reconstruct paths from parent chain |
| `fileSystemInfo` | Mostly | Missing on some remote/"Add Shortcut" items |
| `lastModifiedDateTime` | Mostly | May be missing on API-initiated deletions |
| `createdDateTime` | Mostly | Usually present |
| `@microsoft.graph.downloadUrl` | Yes (files, short-lived) | Expires after ~1 hour |

---

## 4. Implications for Database Schema Design

### 4.1 Fields to Store in Sync Database

Based on the field inventory and reliability analysis, the following fields should be stored:

#### Primary Record (every item)

| Column | Source Field | Type | Notes |
|--------|-------------|------|-------|
| `item_id` | `id` | TEXT PK | Primary key; unique within a drive |
| `drive_id` | `parentReference.driveId` | TEXT | **Must normalize**: lowercase + zero-pad to 16 chars (Personal) |
| `parent_id` | `parentReference.id` | TEXT | Foreign key to parent item |
| `name` | `name` | TEXT | Store from delta; keep local copy for Business deletions |
| `item_type` | Derived from facets | TEXT | `file`, `folder`, `root`, `package`, `remote` |
| `size` | `size` | INTEGER | Store from delta; keep local copy for Personal deletions |
| `etag` | `eTag` | TEXT | Nullable (missing on Business/SP root) |
| `ctag` | `cTag` | TEXT | Nullable (unreliable on Business; never on Business folders) |
| `quick_xor_hash` | `file.hashes.quickXorHash` | TEXT | Nullable; only for files; bogus on deleted items |
| `sha1_hash` | `file.hashes.sha1Hash` | TEXT | Nullable; Personal only |
| `mime_type` | `file.mimeType` | TEXT | Nullable; only for files |
| `created_datetime` | `createdDateTime` | TEXT (ISO8601) | Server-side creation time |
| `modified_datetime` | `lastModifiedDateTime` | TEXT (ISO8601) | Server-side modification time |
| `fs_created_datetime` | `fileSystemInfo.createdDateTime` | TEXT (ISO8601) | Client-reported creation time; nullable |
| `fs_modified_datetime` | `fileSystemInfo.lastModifiedDateTime` | TEXT (ISO8601) | Client-reported modification time; nullable |
| `folder_child_count` | `folder.childCount` | INTEGER | Nullable; only for folders |
| `is_deleted` | Presence of `deleted` facet | BOOLEAN | Track deletion state |
| `local_path` | Computed | TEXT | Reconstructed from parent chain |
| `last_synced` | Client-generated | TEXT (ISO8601) | When we last synced this item |

#### Remote Item Reference (for shared items)

| Column | Source Field | Type | Notes |
|--------|-------------|------|-------|
| `remote_drive_id` | `remoteItem.parentReference.driveId` | TEXT | Normalize like all driveIds |
| `remote_item_id` | `remoteItem.id` | TEXT | ID on the remote drive |
| `remote_name` | `remoteItem.name` | TEXT | May differ from local display name |
| `remote_size` | `remoteItem.size` | INTEGER | |
| `remote_quick_xor_hash` | `remoteItem.file.hashes.quickXorHash` | TEXT | Nullable |

#### Delta State

| Column | Source Field | Type | Notes |
|--------|-------------|------|-------|
| `delta_link` | `@odata.deltaLink` | TEXT | Stored per drive_id |
| `last_delta_time` | Client-generated | TEXT (ISO8601) | When delta was last fetched |

### 4.2 Fields Requiring Normalization Before Storage

| Field | Normalization Required | Reason |
|-------|----------------------|--------|
| `parentReference.driveId` | Lowercase + zero-pad to 16 chars | API casing inconsistency + truncation bug |
| `remoteItem.parentReference.driveId` | Same as above | Same bugs apply |
| `parentReference.path` | URL-decode | API returns `%20` for spaces |
| All driveIds in item ID prefixes | Lowercase | Casing may differ from `parentReference.driveId` |
| Timestamps | Truncate fractional seconds to zero | OneDrive has whole-second precision |
| Timestamps | Validate range | API can return invalid dates |
| `name` | Validate against naming rules | Reject/flag invalid characters |

### 4.3 Fields NOT to Rely on for Change Detection

| Field | Reason |
|-------|--------|
| `cTag` | Omitted in Business delta create/modify; never on Business folders |
| `sha256Hash` | Officially "not supported"; inconsistent availability |
| `crc32Hash` | Only sometimes on Personal; never on Business |
| `parentReference.path` | Never in delta responses; URL-encoded when present |
| `size` (alone) | Can be wrong for SharePoint; missing on Personal deletions |
| `lastModifiedDateTime` (alone) | Can be missing; affected by clock skew; fractional seconds mismatch |
| `file.hashes` on deleted items | Bogus/placeholder values |

### 4.4 Recommended Change Detection Strategy

Use a fallback chain for detecting whether a file has changed:

| Priority | Method | When to Use |
|----------|--------|-------------|
| 1 | `quickXorHash` comparison | Always preferred; available on all account types for files |
| 2 | `eTag` comparison | When hash is missing (zero-byte files, some Business files) |
| 3 | `size` + `lastModifiedDateTime` | When both hash and eTag are unavailable |
| 4 | `size` only | Last resort; for items missing timestamps |

For folders, change detection relies on:
- `eTag` changes (available except on Business/SP root)
- `folder.childCount` changes
- Re-enumeration via delta

### 4.5 Minimal Field Set for Robust Sync

The absolute minimum set of fields needed from the API to perform reliable bidirectional sync:

```
id, name, size, eTag, deleted, file, folder, root,
fileSystemInfo, remoteItem, parentReference, package
```

With the following critical sub-fields:
- `parentReference.id` and `parentReference.driveId` (for tree structure)
- `file.hashes.quickXorHash` (for content verification)
- `fileSystemInfo.lastModifiedDateTime` (for timestamp preservation)

This matches the `$select` clause used by the reference implementation:
```
$select=id,name,eTag,cTag,deleted,file,folder,root,fileSystemInfo,
        remoteItem,parentReference,size,createdBy,lastModifiedBy,package
```

**Note:** `cTag` is included despite its unreliability because it IS useful on Personal accounts and for direct GET requests on Business accounts. `createdBy` and `lastModifiedBy` are included for audit trail and conflict resolution context.

---

## 5. Summary: Key Takeaways

### Five Critical Rules for DriveItem Field Handling

1. **Always normalize driveIds.** Every `driveId` from any API response must be lowercased and (for Personal accounts) zero-padded to 16 characters before storage or comparison. This is the single most impactful defensive measure.

2. **Never rely on `parentReference.path` from delta.** It is never present. Always track items by `id` and reconstruct paths by walking the parent chain in the local database.

3. **QuickXorHash is the only universal file hash.** It is the only hash guaranteed across all account types. Implement local QuickXorHash computation for verification. Ignore hashes on deleted items.

4. **Expect missing fields based on context.** Business deleted items lack `name`. Personal deleted items lack `size`. Business folders and delta create/modify operations lack `cTag`. Business/SharePoint root items lack `eTag`. The database schema must use nullable columns for all of these.

5. **Validate and sanitize all API data.** Timestamps may be invalid. Hashes may be bogus. Sizes may be wrong (SharePoint enrichment, iOS HEIC conversion). The sync engine must treat API metadata as advisory, not authoritative, and have fallback strategies for every field.

### Account Type Cheat Sheet

| Behavior | Personal | Business | SharePoint |
|----------|:--------:|:--------:|:----------:|
| DriveId format | 16 hex chars (buggy) | GUID | GUID |
| DriveId needs normalization | **Yes** (case + padding) | Yes (case only) | Yes (case only) |
| QuickXorHash | Yes | Yes | Yes |
| SHA-1 | Yes | No | No |
| cTag on folders | Yes | **No** | **No** |
| cTag in delta create/modify | Yes | **No** | **No** |
| Name on delta delete | Yes | **No** | **No** |
| Size on delta delete | **No** | Yes | Yes |
| eTag on root | Yes | **No** | **No** |
| sharepointIds | No | Yes | Yes |
| parentReference.path in delta | **Never** | **Never** | **Never** |
| fileSystemInfo on remote items | Usually | Usually | **Sometimes missing** |
| Permanent delete API | No | Yes | Yes |
| Timestamp delta tokens | No | Yes | Yes |
| Delta shared item header needed | Yes (always) | Yes (if enabled) | Yes (if enabled) |

---

## Sources

- [driveItem resource type -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/resources/driveitem?view=graph-rest-1.0)
- [driveItem: delta -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/driveitem-delta?view=graph-rest-1.0)
- [File resource type -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/resources/file?view=graph-rest-1.0)
- [Hashes resource type -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/resources/hashes?view=graph-rest-1.0)
- [itemReference resource type -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/resources/itemreference?view=graph-rest-1.0)
- [fileSystemInfo resource type -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/resources/filesysteminfo?view=graph-rest-1.0)
- [Folder resource type -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/resources/folder?view=graph-rest-1.0)
- [remoteItem resource type -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/resources/remoteitem?view=graph-rest-1.0)
- [deleted resource type -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/resources/deleted?view=graph-rest-1.0)
- [shared resource type -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/resources/shared?view=graph-rest-1.0)
- [sharepointIds resource type -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/resources/sharepointids?view=graph-rest-1.0)
- [specialFolder resource type -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/resources/specialfolder?view=graph-rest-1.0)
- [baseItem resource type -- Microsoft Graph v1.0](https://learn.microsoft.com/en-us/graph/api/resources/baseitem?view=graph-rest-1.0)
- [Best practices for discovering files and detecting changes at scale -- OneDrive dev center](https://learn.microsoft.com/en-us/onedrive/developer/rest-api/concepts/scan-guidance?view=odsp-graph-online)
- abraunegg/onedrive issue tracker (issues #154, #200, #237, #966, #1224, #2053, #2306, #2353, #2433, #2442, #2471, #2562, #2938, #2957, #3030, #3066, #3072, #3115, #3127, #3136, #3165, #3167, #3169, #3180, #3237, #3336)
- [onedrive-api-docs#935](https://github.com/OneDrive/onedrive-api-docs/issues/935), [#1723](https://github.com/OneDrive/onedrive-api-docs/issues/1723), [#1765](https://github.com/OneDrive/onedrive-api-docs/issues/1765), [#1890](https://github.com/OneDrive/onedrive-api-docs/issues/1890), [#1891](https://github.com/OneDrive/onedrive-api-docs/issues/1891), [#1895](https://github.com/OneDrive/onedrive-api-docs/issues/1895)
