# Domain Glossary: OneDrive & Microsoft Graph Concepts

This document defines the shared vocabulary used throughout all project documentation. Every concept is defined precisely to eliminate ambiguity.

---

## Core Resources

### Drive
The top-level container representing a user's OneDrive or a SharePoint document library. Every user has at least one drive (their default drive). Each drive has a unique `id`, a `driveType`, and a `root` item.

### Drive Type
The `driveType` property identifies the kind of drive:
- **`personal`** — OneDrive Personal (consumer Microsoft account)
- **`business`** — OneDrive for Business (work/school account)
- **`documentLibrary`** — SharePoint document library

Drive type affects available features, API behavior, hash types, and rate limits. Many edge cases stem from differences between these three types.

### DriveItem
The fundamental resource in the OneDrive API. Represents a file, folder, or other item stored in a drive. Every object in a drive hierarchy is a DriveItem. DriveItems are identified by a unique `id` within their drive and can also be addressed by path (`/drive/root:/path/to/item`).

### Root Item
The top-most DriveItem in a drive, accessed via `/drive/root`. Has the `root` facet set. The root item is the ancestor of all other items in the drive. In delta queries, the root item is often the starting point.

---

## DriveItem Identity & Versioning

### Item ID
A unique, opaque string identifier for a DriveItem within its drive. Stable across renames and moves within the same drive. The primary key for tracking items. Format varies between Personal and Business accounts.

### eTag
An opaque string that changes whenever **any property** of the item changes (metadata or content). Used for optimistic concurrency control. Can be passed in `If-Match` headers to prevent conflicting updates.

### cTag (Content Tag)
An opaque string that changes only when the **content** of the item changes. Metadata-only changes (rename, move) do not change the cTag.

**Important behavioral differences:**
- Not returned for folders on OneDrive for Business
- Not returned for the root item on OneDrive for Business
- Missing on very old files on OneDrive for Business
- Not returned in delta query responses for create/modify operations on OneDrive for Business
- Not returned in delta query responses for deleted items (neither Business nor Personal)

### fileSystemInfo
Client-provided timestamps (`createdDateTime`, `lastModifiedDateTime`) representing when the file was created/modified on the local filesystem. These are **writable** — a sync client sets these when uploading to preserve original timestamps. Distinct from the server-managed `createdDateTime` and `lastModifiedDateTime` on the DriveItem itself.

---

## Item Types & Facets

### Facet
A set of properties grouped as a JSON object on a DriveItem. Facets indicate the item's type and capabilities. A DriveItem's type is determined by which facets are present (they are mutually indicative — e.g., if `folder` facet exists, the item is a folder).

### File Facet
Present when the DriveItem is a file. Contains `mimeType` and `hashes` (content hashes for integrity verification).

### Folder Facet
Present when the DriveItem is a folder. Contains `childCount` (number of direct children) and optional `view` preferences.

### Package Facet
Present when the DriveItem is a "package" — an item that should be treated as a single unit rather than a folder, even though it may contain children. The primary example is **OneNote notebooks** (`type: "oneNote"`). Sync clients must treat packages as opaque blobs, not recurse into them.

### Remote Item Facet
Present when the DriveItem is a reference to an item on a **different drive**. This occurs with shared items, shared folders, and "Add shortcut to My files" links. The `remoteItem` facet contains the remote item's `id`, `name`, `size`, drive information, and its own facets. A remote item has both a local identity (in the user's drive) and a remote identity (in the source drive).

### Shared Facet
Present when the item has been shared. Contains `sharedDateTime` and `owner` information. Note: the `shared` facet indicates the item was shared *with* the current user, not that the current user shared it.

### Deleted Facet
Present when the item has been deleted. Contains a `state` field (typically `"deleted"`). Items with this facet appear in delta responses to signal that the client should remove them locally. The `name` property may be missing on deleted items in OneDrive for Business.

### Special Folder
A well-known folder with a canonical name. Examples: `documents`, `photos`, `cameraroll`, `approot`, `music`. Accessed via `/drive/special/{name}`. The `specialFolder` facet on a DriveItem indicates it is one of these.

---

## Content Hashes

### QuickXorHash
A proprietary Microsoft hash algorithm. The **only hash guaranteed to be available** on both OneDrive Personal and OneDrive for Business. Returned as a Base64-encoded string. Used as the primary mechanism for content verification in sync clients.

### SHA1 Hash
Available on OneDrive Personal only (not Business). Returned as a hex string. Useful for verification but cannot be relied upon cross-platform.

### SHA256 Hash
Available on OneDrive for Business (SharePoint). Not available on Personal. Returned as a hex string.

### CRC32 Hash
Available on OneDrive Personal. Returned as a hex string (little-endian). Not available on Business.

**Hash availability summary:**

| Hash | Personal | Business |
|------|----------|----------|
| QuickXorHash | Yes | Yes |
| SHA1 | Yes | No |
| SHA256 | No | Yes |
| CRC32 | Yes | No |

---

## Parent Reference & Navigation

### Parent Reference (itemReference)
Metadata about an item's parent container. Contains:
- **`driveId`** — the drive containing the parent
- **`id`** — the parent item's ID
- **`path`** — percent-encoded API path (e.g., `/drive/root:/Documents`)
- **`driveType`** — type of drive containing the parent
- **`siteId`** — SharePoint site ID (Business/SharePoint only)
- **`shareId`** — identifier for shared resource access

**Important:** In delta query responses, the `path` property in `parentReference` is **not included**. Items must be tracked by `id`, not by path. Renames of parent folders do not trigger delta entries for descendants.

### Addressing DriveItems
Two primary addressing modes:
1. **By ID:** `GET /drives/{driveId}/items/{itemId}` — stable, preferred for sync
2. **By Path:** `GET /drive/root:/path/to/file` — human-readable, but fragile across renames

---

## Delta Sync Concepts

### Delta Query
The mechanism for efficiently tracking changes to a drive. Instead of listing all items, delta returns only items that have changed since a given point in time. The entry point is `GET /drives/{driveId}/root/delta`.

### Delta Token
An opaque string embedded in delta response URLs that represents a point in time. The client saves this token and uses it on subsequent requests to get only newer changes. Tokens can expire or be invalidated by the server.

### nextLink
A URL in a delta response (`@odata.nextLink`) indicating there are more pages of results in the current changeset. The client must follow all nextLinks until none remain.

### deltaLink
A URL in a delta response (`@odata.deltaLink`) returned on the **last page** of results. Contains the delta token for future sync cycles. The client saves this URL and calls it next time to get only new changes.

### Delta Token Invalidation
When a delta token is too old or the server state has changed significantly, the server returns **HTTP 410 Gone** with an error code and a `Location` header pointing to a new URL for fresh enumeration.

Two resync error types:
- **`resyncChangesApplyDifferences`** — server was up-to-date when you last synced; replace local items with server versions, upload local changes server doesn't know about
- **`resyncChangesUploadDifferences`** — upload local items server didn't return; keep both copies when unsure which is newer

### Initial Enumeration
The first delta call (no token) returns **every item** in the drive hierarchy, paginated via nextLinks. This establishes the client's initial state. The final page includes a deltaLink for subsequent incremental syncs.

### Latest Token
Calling delta with `?token=latest` returns an empty result set and a deltaLink. Useful when the client wants to start tracking changes from "now" without enumerating existing items. The client will only see changes made after this call.

### Timestamp Token
On OneDrive for Business/SharePoint only, a URL-encoded timestamp can be used as a token (e.g., `?token=2021-09-29T20%3A00%3A00Z`) to get changes since that time. Not available on Personal.

---

## Delta Response Behavior

### Item Deduplication
The same item may appear **multiple times** in a delta response. The client must use the **last occurrence** — it represents the most recent state.

### Deleted Items in Delta
Deleted items appear with the `deleted` facet. On deletion:
- **Business:** `cTag` and `name` are omitted
- **Personal:** `cTag` and `size` are omitted

**Important:** Only delete a folder locally if it is empty after processing all changes in the response.

### Property Omissions
Delta responses may omit certain properties:
- **Business create/modify:** `cTag` is omitted
- **Business delete:** `cTag` and `name` are omitted
- **Personal delete:** `cTag` and `size` are omitted

### Path Not Included
The `parentReference.path` property is **never included** in delta responses. This means the sync engine must maintain its own path-to-ID mapping by tracking parent-child relationships through `parentReference.id`.

---

## Concurrency & Conflict

### Conflict Behavior
The `@microsoft.graph.conflictBehavior` instance annotation controls what happens when creating/uploading an item whose name collides with an existing item:
- **`fail`** — return an error
- **`replace`** — overwrite the existing item
- **`rename`** — auto-rename the new item (e.g., `file (1).txt`)

Default for PUT is `replace`. Passed as a URL parameter, not in the request body.

### Optimistic Concurrency
Using `If-Match: {eTag}` headers on update/delete requests ensures the operation only succeeds if the item hasn't been modified since the eTag was retrieved. If it has changed, the server returns **412 Precondition Failed**.

---

## Upload Concepts

### Simple Upload
Direct PUT of file content to `/drive/items/{id}/content`. Limited to files up to 4 MB.

### Upload Session (Resumable Upload)
For files over 4 MB (up to 250 GB on Business, 100 GB on Personal). Process:
1. Create session: `POST /drive/items/{id}/createUploadSession`
2. Upload chunks (fragments) to the session URL
3. Each chunk response indicates `nextExpectedRanges`
4. Session URL has built-in authentication (no Bearer token needed)
5. Sessions expire (typically after a few days)

### Download URL
The `@microsoft.graph.downloadUrl` instance annotation provides a pre-authenticated, short-lived URL (valid ~1 hour) for downloading file content. Does not require an Authorization header. Cannot be cached long-term.

---

## Sharing & Permissions

### Permission
Represents access rights on a DriveItem. Has `roles` (read, write, owner), and may be granted directly to a user, via a sharing link, or inherited from a parent.

### Sharing Link
A URL-based permission. Types: `view` (read-only), `edit` (read-write), `embed` (for web embedding). Scopes: `anonymous` (anyone), `organization` (org members), `users` (specific users).

### Inherited Permission
A permission that flows down from a parent folder. The `inheritedFrom` property indicates the source item. Inherited permissions cannot be removed from the child — they must be removed from the ancestor.

---

## Remote Items & Shared Folders

### Remote Item
A DriveItem that exists on a different drive from the one being queried. When a user adds a shared folder to their drive ("Add shortcut to My files"), a remote item appears in their drive with a `remoteItem` facet pointing to the actual item on the source drive.

### Remote Drive ID
The `remoteDriveId` in the reference implementation's data model — the drive ID of the source drive where the shared item actually lives.

### Remote Item ID
The `remoteId` — the item ID of the actual item on the source drive. To perform operations on the actual content, you must address the item using `remoteDriveId` and `remoteId`.

### Actual Online Name
In the reference implementation, the `remoteName` / `actualOnlineName` field tracks the name of the item on the remote drive, which may differ from its display name in the user's drive. Only relevant for OneDrive for Business shared folders.

---

## Account & Authentication

### Microsoft Account (MSA)
Consumer account for OneDrive Personal. Uses `login.microsoftonline.com` for auth.

### Work or School Account (AAD/Entra ID)
Organizational account for OneDrive for Business. Uses Azure Active Directory (now Entra ID) for auth.

### Device Code Flow
OAuth2 authentication flow where the app displays a code and URL, the user authenticates in a browser on any device, and the app polls for the token. Used by CLI clients that can't open a browser directly.

### National Cloud
Microsoft cloud deployments in specific geographies with separate endpoints:
- **Global:** `graph.microsoft.com`
- **US Government:** `graph.microsoft.us`
- **China (21Vianet):** `microsoftgraph.chinacloudapi.cn`
- **Germany:** `graph.microsoft.de` (deprecated)

Each national cloud has its own auth endpoints and may have different feature availability.

---

## Rate Limiting & Throttling

### HTTP 429 (Too Many Requests)
Returned when the client exceeds rate limits. The `Retry-After` header specifies seconds to wait before retrying.

### HTTP 503 (Service Unavailable)
May also indicate throttling. Handle similarly to 429.

### HTTP 509 (Bandwidth Limit Exceeded)
Specific to SharePoint. Indicates the app has exceeded available bandwidth.

---

## Sync Engine Concepts (from reference implementation)

### Item Type
The reference implementation classifies items as: `file`, `dir` (directory/folder), `remote` (shared item from another drive), `root` (the drive root), or `unknown`.

### Sync Status
A per-item tracking field in the reference implementation's database. Tracks whether an item is synced, needs upload, needs download, or is in a conflict state.

### Item Database
A local SQLite database maintaining the state of all known items. Stores item metadata (ID, name, type, eTags, hashes, parent relationships, remote references) and is the sync engine's source of truth for what the local state "should" look like.

### Relocation Fields
The reference implementation tracks `relocDriveId` and `relocParentId` — fields used during move/rename operations to record where an item is being relocated to, enabling the sync engine to handle moves atomically.

---

## Important Behavioral Notes

1. **Name may be missing** on deleted items in OneDrive for Business
2. **eTag not returned** for the root item in OneDrive for Business
3. **cTag not returned** for folders in OneDrive for Business
4. **fileSystemInfo may be missing** on remote items created via "Add Shortcut" in OneDrive WebUI
5. **lastModifiedDateTime not returned** for items deleted by OneDrive itself (API change documented in OneDrive API docs issue #834)
6. **Invalid timestamps** can be returned by the API — sync clients must validate and fall back to current time
7. **Path-based addressing is fragile** — after a rename, old paths break; ID-based addressing is stable
8. **Items may appear multiple times** in delta responses — always use the last occurrence
9. **Delta does not return descendant entries** when a parent folder is renamed — the sync engine must infer path changes from parent relationships
10. **Folder deletion in delta** — only remove a local folder after confirming it's empty post-sync
