# Microsoft Graph API Analysis for OneDrive Sync Client

This document provides a comprehensive analysis of every Microsoft Graph API endpoint required to build a fully functional OneDrive sync client. It is derived from the official Microsoft Graph API documentation and observed behaviors documented in the reference implementation.

**Audience:** Tier 2 sync engine designers who will use this as a specification for implementing API interactions in Go.

---

## Table of Contents

1. [Core Endpoints for Sync](#1-core-endpoints-for-sync)
2. [Supporting Endpoints](#2-supporting-endpoints)
3. [Authentication Endpoints](#3-authentication-endpoints)
4. [Rate Limiting and Throttling](#4-rate-limiting-and-throttling)
5. [Personal vs Business vs SharePoint Differences](#5-personal-vs-business-vs-sharepoint-differences)
6. [Edge Cases and Gotchas](#6-edge-cases-and-gotchas)

---

## 1. Core Endpoints for Sync

### 1.1 Delta Query (Change Tracking)

The delta query is the single most critical endpoint for a sync client. It enables efficient incremental synchronization by returning only items that have changed since the last sync.

#### Endpoint

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL (default drive)** | `/me/drive/root/delta` |
| **URL (specific drive)** | `/drives/{driveId}/root/delta` |
| **URL (by item)** | `/drives/{driveId}/items/{itemId}/delta` |
| **Auth** | Bearer token required |

#### Parameters

| Parameter | Type | Description |
|---|---|---|
| `token` | string (query) | Opaque delta token. Omit for initial full enumeration. Use `latest` to get only future changes without enumerating existing items. Use a URL-encoded ISO 8601 timestamp on Business/SharePoint only. |
| `$select` | string (query) | Comma-separated list of properties to return. Reduces payload size. |
| `$top` | integer (query) | Maximum number of items per page. |

**Recommended `$select` clause** (from reference implementation):
```
id,name,eTag,cTag,deleted,file,folder,root,fileSystemInfo,remoteItem,parentReference,size,createdBy,lastModifiedBy,package
```

#### Request Headers

| Header | Value | Purpose |
|---|---|---|
| `Authorization` | `Bearer {token}` | Authentication |
| `Prefer` | `Include-Feature=AddToOneDrive` | Required to receive shared folder items in the delta response. Needed for Personal accounts (always) and Business accounts (when syncing shared items). Added as a requirement from March 2025 onward. |
| `Prefer` | `hierarchicalsharing` | Optimizes permission change information in responses. |
| `deltaExcludeParent` | (any value) | If present, parent items of changed items are excluded from the response. |

#### Response Shape

```json
{
  "value": [
    {
      "id": "...",
      "name": "...",
      "folder": {},
      "file": { "hashes": { "quickXorHash": "..." } },
      "deleted": { "state": "deleted" },
      "parentReference": { "driveId": "...", "id": "...", "driveType": "..." },
      "fileSystemInfo": { "createdDateTime": "...", "lastModifiedDateTime": "..." },
      "remoteItem": {},
      "size": 12345,
      "eTag": "...",
      "cTag": "..."
    }
  ],
  "@odata.nextLink": "https://graph.microsoft.com/v1.0/...",
  "@odata.deltaLink": "https://graph.microsoft.com/v1.0/...?token=..."
}
```

#### Pagination and Token Management

**Initial Enumeration:**
1. Call `/drives/{driveId}/root/delta` without a token.
2. The API returns pages of all items with `@odata.nextLink`.
3. Follow every `@odata.nextLink` until `@odata.deltaLink` appears.
4. Store the `@odata.deltaLink` (it contains the opaque delta token).

**Incremental Sync:**
1. Call the stored `@odata.deltaLink` URL directly.
2. Process all returned items (changed since last sync).
3. Follow `@odata.nextLink` pages until `@odata.deltaLink` appears.
4. Replace the stored deltaLink with the new one.

**Critical implementation notes:**
- The `@odata.nextLink` and `@odata.deltaLink` are complete URLs. Follow them as-is; do not construct them manually.
- The `token` query parameter can be extracted from a deltaLink URL and passed directly as `?token=<value>` when constructing delta calls manually.
- Each response page may contain hundreds of items. Process them before requesting the next page to manage memory.
- The same item may appear more than once across pages. **Always use the last occurrence** of any given item ID.

#### Token Expiry and Resync (HTTP 410 Gone)

When a stored deltaLink token expires or becomes invalid, the API returns:

```
HTTP/1.1 410 Gone
Location: <new deltaLink URL for full re-enumeration>
```

The error response body contains a `code` field with one of two resync types:

| Error Code | Meaning | Action Required |
|---|---|---|
| `resyncChangesApplyDifferences` | Server state is authoritative. | Replace local items with server versions (including deletes). Upload any local changes the server does not know about. |
| `resyncChangesUploadDifferences` | Keep both copies if uncertain. | Upload local items the server did not return. Upload files that differ, keeping both copies if unsure which is newer. |

**Implementation guidance:**
- On HTTP 410, discard the stored deltaLink and perform a full re-enumeration.
- The reference implementation handles this by catching the 410 exception and retrying with an empty deltaLink (full scan).
- After a full re-enumeration completes, compare the full server state against local state to detect conflicts.

#### `token=latest` Shortcut

Calling `/drives/{driveId}/root/delta?token=latest` returns an empty `value` array and a `@odata.deltaLink` for the current point in time. This is useful when:
- You want to skip initial enumeration entirely and only track future changes.
- You have already performed a full enumeration via another method (e.g., listing children recursively).

**Warning:** If you need a full local representation, you must perform initial enumeration. `token=latest` will not return existing items.

#### Timestamp Tokens (Business/SharePoint Only)

On OneDrive for Business and SharePoint, you can pass a URL-encoded ISO 8601 timestamp as the token value:
```
GET /me/drive/root/delta?token=2021-09-29T20%3A00%3A00Z
```
This returns all changes since that timestamp. This feature is **not supported on OneDrive Personal**.

#### Property Omissions in Delta Responses

Delta responses may omit certain properties depending on the operation type and account type:

| Account Type | Operation | Properties Omitted |
|---|---|---|
| OneDrive for Business | Create/Modify | `cTag` |
| OneDrive for Business | Delete | `cTag`, `name` |
| OneDrive Personal | Create/Modify | (none) |
| OneDrive Personal | Delete | `cTag`, `size` |

**Critical implication:** On Business accounts, deleted items may not have a `name` property. The sync engine must track items by `id`, not by name or path.

#### Item Deduplication

The delta feed shows the latest state for each item, not each individual change. If an item was renamed twice between delta calls, it appears once with its final name. However, the same item ID may appear multiple times within a single delta response (across pages). The sync engine must deduplicate by always using the last occurrence of each item ID.

#### `parentReference.path` Not Returned

The `parentReference` property on items in a delta response **does not include the `path` field**. This is by design: renaming a folder does not cause its descendants to reappear in the delta feed. The sync engine **must track items by `id`** and reconstruct paths from the parent chain in its local database.

---

### 1.2 Get Item by ID

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL** | `/drives/{driveId}/items/{itemId}` |
| **URL (default drive)** | `/me/drive/items/{itemId}` |
| **Response** | Single `DriveItem` JSON object |

**Optional `$select` query parameter:** Reduces response payload. The reference implementation uses:
```
?select=id,name,eTag,cTag,deleted,file,folder,root,fileSystemInfo,remoteItem,parentReference,size,createdBy,lastModifiedBy,webUrl,lastModifiedDateTime,package
```

**Use cases in sync:**
- Resolving item details when delta responses omit properties.
- Verifying server state before upload or conflict resolution.
- Fetching `@microsoft.graph.downloadUrl` for file downloads.

---

### 1.3 Get Item by Path

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL (root)** | `/me/drive/root` |
| **URL (path)** | `/me/drive/root:/{path}:` (note trailing colon) |
| **URL (specific drive)** | `/drives/{driveId}/root:/{path}:` |
| **Response** | Single `DriveItem` JSON object |

**Path encoding:** The path must be URL-encoded. Special characters like `'` need OData escaping (doubled) and then URL encoding.

---

### 1.4 List Children

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL (by ID)** | `/drives/{driveId}/items/{itemId}/children` |
| **URL (root)** | `/me/drive/root/children` |
| **URL (by path)** | `/me/drive/root:/{path}:/children` |
| **Response** | `{ "value": [...], "@odata.nextLink": "..." }` |

**Optional parameters:** `$select`, `$top`, `$orderby`, `$filter`, `$expand`.

**Pagination:** Follow `@odata.nextLink` until it disappears.

**Important note from Microsoft:** Listing children is NOT a substitute for delta queries during sync. Writes occurring during enumeration may cause items to be missed. Delta queries are the only reliable method for complete enumeration.

The reference implementation uses the same `$select` clause and `Prefer: Include-Feature=AddToOneDrive` header as delta queries for consistency.

---

### 1.5 Upload Content

There are two upload methods:

#### 1.5.1 Simple Upload (files up to 4 MB)

| Property | Value |
|---|---|
| **Method** | `PUT` |
| **URL (new file by path)** | `/drives/{driveId}/items/{parentId}:/{filename}:/content` |
| **URL (replace by ID)** | `/drives/{driveId}/items/{itemId}/content` |
| **Content-Type** | `application/octet-stream` |
| **Request Body** | Raw file bytes |
| **Response** | `DriveItem` JSON object of the created/updated file |
| **Max Size** | 4 MB (4,194,304 bytes) |

**Response codes:** `200 OK` (replaced existing) or `201 Created` (new file).

#### 1.5.2 Resumable Upload Session (files larger than 4 MB)

**Step 1: Create Upload Session**

| Property | Value |
|---|---|
| **Method** | `POST` |
| **URL (new file)** | `/drives/{driveId}/items/{parentId}:/{filename}:/createUploadSession` |
| **URL (update existing)** | `/drives/{driveId}/items/{itemId}/createUploadSession` |
| **Content-Type** | `application/json` |
| **Request Body** | Optional: `{ "item": { "@microsoft.graph.conflictBehavior": "rename" }, "deferCommit": false }` |
| **Response** | `{ "uploadUrl": "...", "expirationDateTime": "..." }` |

Optional request headers:
- `If-Match: {eTag}` -- conditional creation; returns 412 if item was modified.
- `If-None-Match: {eTag}` -- returns 412 if item matches.

**Known issue from reference implementation:** Using `If-Match` with eTag on upload session creation can cause the eTag to change during the session, leading to `412 Precondition Failed` followed by `416 Requested Range Not Satisfiable` during fragment upload. The reference implementation comments out this header as a workaround.

**Step 2: Upload Byte Ranges (Fragments)**

| Property | Value |
|---|---|
| **Method** | `PUT` |
| **URL** | The `uploadUrl` from Step 1 |
| **Content-Length** | Size of this fragment in bytes |
| **Content-Range** | `bytes {start}-{end}/{totalSize}` (inclusive range) |
| **Request Body** | Raw bytes for this fragment |
| **Auth** | **DO NOT** include `Authorization` header. The uploadUrl is pre-authenticated. Including auth headers may cause `401 Unauthorized`. |

**Fragment size requirements:**
- Each fragment **MUST** be a multiple of 320 KiB (327,680 bytes), except for the final fragment.
- Maximum fragment size: 60 MiB per PUT request.
- Recommended fragment size: 5-10 MiB for optimal throughput.
- Fragments must be uploaded sequentially in order.

**Intermediate response (more fragments needed):**
```json
HTTP/1.1 202 Accepted
{
  "expirationDateTime": "2025-01-29T09:21:55.523Z",
  "nextExpectedRanges": ["26-"]
}
```

**Final response (upload complete):**
```json
HTTP/1.1 201 Created
{
  "id": "...",
  "name": "largefile.dat",
  "size": 128,
  "file": {}
}
```

**Step 3: Resuming Interrupted Uploads**

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL** | The `uploadUrl` from Step 1 |
| **Response** | `{ "expirationDateTime": "...", "nextExpectedRanges": ["12345-"] }` |

If a fragment upload fails or is interrupted, GET the upload URL to discover which ranges have been received, then resume from the missing range.

**Step 4: Cancel Upload Session**

| Property | Value |
|---|---|
| **Method** | `DELETE` |
| **URL** | The `uploadUrl` from Step 1 |
| **Response** | `204 No Content` |

**Session lifecycle notes:**
- Upload sessions expire if inactive. The `expirationDateTime` is returned in every response.
- Each successfully uploaded fragment extends the expiration time.
- If `fileSize` exceeds available quota, `507 Insufficient Storage` is returned at session creation.
- On name conflict at final commit: `409 Conflict` with `nameAlreadyExists` error code.
- Handle `404 Not Found` on resume by starting a completely new upload session.

**Long uploads and token expiry:** The reference implementation pre-emptively checks access token expiry before each fragment PUT to avoid `401` errors during multi-hour uploads. This is critical for very large files.

---

### 1.6 Download Content

Two primary download mechanisms:

#### 1.6.1 Content Endpoint (with redirect)

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL (by ID)** | `/drives/{driveId}/items/{itemId}/content` |
| **URL (by path)** | `/me/drive/root:/{path}:/content` |
| **Typical Response** | `302 Found` with `Location` header pointing to pre-authenticated download URL |

The redirect URL is a short-lived, pre-authenticated URL hosted on a CDN (e.g., `*.1drv.com`). **Do not send `Authorization` headers** to this URL.

The reference implementation appends `?AVOverride=1` to the content URL, though this parameter's purpose is undocumented (possibly bypasses antivirus scanning for known-safe files).

#### 1.6.2 `@microsoft.graph.downloadUrl` from Item Metadata

When you GET an item's metadata, the response includes:
```json
"@microsoft.graph.downloadUrl": "https://..."
```

This is a pre-authenticated, short-lived download URL.

**Expiry:** This URL expires after approximately 1 hour. Do not cache or reuse it across sync cycles.

**Fallback strategy:** The reference implementation first attempts the `/content` redirect approach. If that returns `401` or `404`, it falls back to fetching item metadata and using `@microsoft.graph.downloadUrl`.

#### 1.6.3 Range Downloads (Resumable)

Range requests are supported on both the `/content` redirect URL and `@microsoft.graph.downloadUrl`:

```
GET {downloadUrl}
Range: bytes=12345-
```

Response: `206 Partial Content` with the requested byte range.

The reference implementation uses `.partial` file suffixes during download and maintains resume state in JSON files containing `driveId`, `itemId`, `onlineHash`, `originalFilename`, `downloadFilename`, and `resumeOffset`.

#### 1.6.4 Format Conversion Download

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL** | `/me/drive/root:/{path}:/content?format={format}` |
| **Supported formats** | `pdf`, `jpg`, `png`, `html`, others depending on file type |

Not relevant for sync but useful for auxiliary features.

---

### 1.7 Create Folder

| Property | Value |
|---|---|
| **Method** | `POST` |
| **URL (by parent ID)** | `/drives/{driveId}/items/{parentId}/children` |
| **URL (by parent path)** | `/me/drive/root:/{parentPath}:/children` |
| **URL (root)** | `/me/drive/root/children` |
| **Content-Type** | `application/json` |
| **Response** | `DriveItem` of the created folder (`201 Created`) |

**Request body:**
```json
{
  "name": "New Folder Name",
  "folder": {},
  "@microsoft.graph.conflictBehavior": "rename"
}
```

**Conflict behavior options:**
- `fail` (default) -- returns `409 Conflict` if name exists.
- `rename` -- auto-appends a number suffix.
- `replace` -- replaces the existing item.

**Note:** `@microsoft.graph.conflictBehavior` is sent in the **request body**, not as a URL parameter, for folder creation. This differs from some other operations.

---

### 1.8 Update Item (PATCH)

| Property | Value |
|---|---|
| **Method** | `PATCH` |
| **URL** | `/drives/{driveId}/items/{itemId}` |
| **Content-Type** | `application/json` |
| **Response** | Updated `DriveItem` (`200 OK`) |

**Use cases:**
- **Rename:** `{ "name": "newname.txt" }`
- **Move:** `{ "parentReference": { "id": "{newParentId}" } }`
- **Move using path:** `{ "parentReference": { "path": "/drive/root:/NewFolder" } }`
- **Update timestamps:** `{ "fileSystemInfo": { "lastModifiedDateTime": "2024-01-01T00:00:00Z" } }`
- **Rename + Move combined:** `{ "name": "newname.txt", "parentReference": { "id": "{newParentId}" } }`

**Conditional update:** Use `If-Match: {eTag}` header to ensure no concurrent modifications. Returns `412 Precondition Failed` if the eTag does not match.

**Known issue:** The reference implementation notes that using `If-Match` with `DELETE` operations always fails with `412 Precondition Failed` -- this needs investigation.

---

### 1.9 Delete Item

| Property | Value |
|---|---|
| **Method** | `DELETE` |
| **URL** | `/drives/{driveId}/items/{itemId}` |
| **Response** | `204 No Content` on success |

This moves items to the OneDrive recycle bin (soft delete). Items can be restored from the recycle bin through the web UI.

**Permanent delete (Business/SharePoint only):**

| Property | Value |
|---|---|
| **Method** | `POST` |
| **URL** | `/drives/{driveId}/items/{itemId}/permanentDelete` |
| **Request Body** | Empty (zero content length) |
| **Response** | `204 No Content` |

**Important:** The permanentDelete API is **not supported** on OneDrive Personal accounts.

---

### 1.10 Copy Item (Async)

| Property | Value |
|---|---|
| **Method** | `POST` |
| **URL** | `/drives/{driveId}/items/{itemId}/copy` |
| **Content-Type** | `application/json` |
| **Response** | `202 Accepted` with `Location` header containing monitor URL |

**Request body:**
```json
{
  "parentReference": {
    "driveId": "{destDriveId}",
    "id": "{destFolderId}"
  },
  "name": "optional new name"
}
```

Alternatively, the parent reference can use path format:
```json
{
  "parentReference": {
    "path": "/drive/root:/DestinationFolder"
  }
}
```

**Monitoring the copy operation:**
```
GET {monitorUrl}
```

Response:
```json
{
  "status": "inProgress",
  "percentageComplete": 45,
  "resourceId": "...",
  "resourceLocation": "..."
}
```

Status values: `notStarted`, `inProgress`, `completed`, `failed`, `waiting`.

**Important:** The monitor URL is pre-authenticated. Do **not** send `Authorization` headers.

---

### 1.11 Move Item

Moving an item is implemented as a PATCH that changes the `parentReference`:

| Property | Value |
|---|---|
| **Method** | `PATCH` |
| **URL** | `/drives/{driveId}/items/{itemId}` |
| **Content-Type** | `application/json` |
| **Response** | Updated `DriveItem` (`200 OK`) |

**Request body:**
```json
{
  "parentReference": {
    "id": "{newParentId}"
  }
}
```

Or using path:
```json
{
  "parentReference": {
    "path": "/drive/root:/NewParentFolder"
  }
}
```

Cross-drive moves are not supported as a single operation. Use copy + delete instead.

---

## 2. Supporting Endpoints

### 2.1 Get Drive

| Endpoint | URL | Description |
|---|---|---|
| Default drive | `GET /me/drive` | Current user's default OneDrive |
| Drive by ID | `GET /drives/{driveId}` | Specific drive by ID |
| Drive quota | `GET /drives/{driveId}?$select=quota` | Quota info only |
| Drive root | `GET /drives/{driveId}/root` | Root item of a drive |

**Response (Drive):**
```json
{
  "id": "...",
  "name": "OneDrive",
  "driveType": "personal",
  "owner": { "user": { "displayName": "...", "id": "..." } },
  "quota": {
    "total": 5368709120,
    "used": 1234567890,
    "remaining": 4134141230,
    "state": "normal"
  }
}
```

**Quota states:** `normal`, `nearing`, `critical`, `exceeded`.

---

### 2.2 List Drives

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL** | `/me/drives` |
| **URL (other user)** | `/users/{userId}/drives` |
| **Response** | `{ "value": [ Drive, ... ] }` |

Returns all drives the user has access to, including personal OneDrive, OneDrive for Business, and SharePoint document libraries.

---

### 2.3 Special Folders

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL** | `/me/drive/special/{name}` |
| **Valid names** | `documents`, `photos`, `cameraroll`, `approot`, `music`, `desktop`, `downloads`, `videos` |
| **Response** | `DriveItem` of the special folder |

---

### 2.4 Search

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL (drive-wide)** | `/me/drive/root/search(q='{query}')` |
| **URL (in folder)** | `/drives/{driveId}/items/{itemId}/search(q='{query}')` |
| **URL (specific drive)** | `/drives/{driveId}/root/search(q='{query}')` |
| **Response** | Paginated `DriveItemList` |

**Search within SharePoint sites:**

| Property | Value |
|---|---|
| **URL** | `/sites?search=*` (list all sites) |
| **URL** | `/sites/{siteId}/drives` (list site drives) |

The query string within `search(q='...')` needs OData single-quote escaping (double the quotes: `''`) and URL encoding.

---

### 2.5 Permissions

| Operation | Method | URL |
|---|---|---|
| List permissions | `GET` | `/drives/{driveId}/items/{itemId}/permissions` |
| Get permission | `GET` | `/drives/{driveId}/items/{itemId}/permissions/{permId}` |
| Create sharing link | `POST` | `/drives/{driveId}/items/{itemId}/createLink` |
| Invite users | `POST` | `/drives/{driveId}/items/{itemId}/invite` |
| Update permission | `PATCH` | `/drives/{driveId}/items/{itemId}/permissions/{permId}` |
| Delete permission | `DELETE` | `/drives/{driveId}/items/{itemId}/permissions/{permId}` |

**Create link request body:**
```json
{
  "type": "view",
  "scope": "anonymous",
  "password": "optional"
}
```

Link types: `view`, `edit`, `embed`.
Scopes: `anonymous`, `organization`.

---

### 2.6 Subscriptions / Webhooks

Webhooks provide near-real-time change notifications, complementing delta polling.

#### Create Subscription

| Property | Value |
|---|---|
| **Method** | `POST` |
| **URL** | `/v1.0/subscriptions` |
| **Content-Type** | `application/json` |

**Request body:**
```json
{
  "changeType": "updated",
  "notificationUrl": "https://your-callback.example.com/webhook",
  "resource": "/drives/{driveId}/root",
  "expirationDateTime": "2025-02-17T00:00:00Z",
  "clientState": "{random-uuid}"
}
```

#### Renew Subscription

| Property | Value |
|---|---|
| **Method** | `PATCH` |
| **URL** | `/v1.0/subscriptions/{subscriptionId}` |

**Request body:**
```json
{
  "expirationDateTime": "2025-02-18T00:00:00Z"
}
```

#### Delete Subscription

| Property | Value |
|---|---|
| **Method** | `DELETE` |
| **URL** | `/v1.0/subscriptions/{subscriptionId}` |

#### WebSocket Endpoint (Socket.IO)

For near-real-time push notifications without a public callback URL:

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL (default drive)** | `/v1.0/me/drive/root/subscriptions/socketIo` |
| **URL (specific drive)** | `/v1.0/drives/{driveId}/root/subscriptions/socketIo` |

Returns a WebSocket URL for subscribing to change events. Requires libcurl 7.86.0+ for WebSocket support.

---

### 2.7 Shared With Me

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL** | `/me/drive/sharedWithMe` |
| **Response** | `DriveItemList` of items shared with the current user |

---

### 2.8 Thumbnails

| Operation | Method | URL |
|---|---|---|
| List thumbnails | `GET` | `/drives/{driveId}/items/{itemId}/thumbnails` |
| Get specific size | `GET` | `/drives/{driveId}/items/{itemId}/thumbnails/{setId}/{size}` |

Standard sizes: `small`, `medium`, `large`. Custom sizes: `c{width}x{height}` (e.g., `c200x200`).

**Relevance for sync:** Thumbnails are generally not needed for file sync operations. They may be relevant if the client has a file browser UI.

---

### 2.9 File Versions

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL** | `/drives/{driveId}/items/{itemId}/versions` |
| **Response** | `{ "value": [ DriveItemVersion, ... ] }` |

---

### 2.10 User Profile

| Property | Value |
|---|---|
| **Method** | `GET` |
| **URL** | `/me` |
| **Response** | `User` object with `displayName`, `userPrincipalName`, `id` |

Useful for initial connectivity verification and user identification.

---

## 3. Authentication Endpoints

### 3.1 OAuth2 Endpoints

All authentication uses Azure AD v2.0 endpoints. The specific endpoint URLs depend on the national cloud deployment.

#### Global (Default)

| Endpoint | URL |
|---|---|
| Authorization | `https://login.microsoftonline.com/{tenantId}/oauth2/v2.0/authorize` |
| Token | `https://login.microsoftonline.com/{tenantId}/oauth2/v2.0/token` |
| Device Code | `https://login.microsoftonline.com/{tenantId}/oauth2/v2.0/devicecode` |
| Graph API | `https://graph.microsoft.com` |

For consumer accounts, use `common` as the tenant ID.

#### National Cloud Endpoints

| Cloud | Auth Endpoint | Graph Endpoint |
|---|---|---|
| **Global** | `https://login.microsoftonline.com` | `https://graph.microsoft.com` |
| **US Government L4** | `https://login.microsoftonline.us` | `https://graph.microsoft.us` |
| **US Government L5 (DOD)** | `https://login.microsoftonline.us` | `https://dod-graph.microsoft.us` |
| **Germany** | `https://login.microsoftonline.de` | `https://graph.microsoft.de` |
| **China (21Vianet)** | `https://login.chinacloudapi.cn` | `https://microsoftgraph.chinacloudapi.cn` |

**Implementation note:** The redirect URL has a special case -- when using a national cloud endpoint with the default application ID (not a custom one), the redirect URL should still use the global auth endpoint. This is a quirk observed in the reference implementation.

### 3.2 Device Code Flow

The preferred authentication method for CLI applications.

**Step 1: Request Device Code**

```
POST https://login.microsoftonline.com/common/oauth2/v2.0/devicecode
Content-Type: application/x-www-form-urlencoded

client_id={clientId}&scope=offline_access files.readwrite.all user.read email openid profile
```

**Response:**
```json
{
  "user_code": "ABCDEFGH",
  "device_code": "...",
  "verification_uri": "https://microsoft.com/devicelogin",
  "expires_in": 900,
  "interval": 5,
  "message": "To sign in, use a web browser to open..."
}
```

**Step 2: Poll for Token**

```
POST https://login.microsoftonline.com/common/oauth2/v2.0/token
Content-Type: application/x-www-form-urlencoded

client_id={clientId}&grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code={deviceCode}
```

Poll every `interval` seconds. Handle these error responses:

| Error | Meaning |
|---|---|
| `authorization_pending` | User hasn't completed login yet. Keep polling. |
| `authorization_declined` | User denied the request. Stop polling. |
| `expired_token` | Device code expired. Restart the flow. |

**Step 3: Token Response**

```json
{
  "access_token": "...",
  "refresh_token": "...",
  "token_type": "Bearer",
  "expires_in": 3600
}
```

### 3.3 Token Refresh

Token refresh is handled automatically by the OAuth2 library (Go's `golang.org/x/oauth2`). The `TokenSource` wrapper detects when the access token changes and persists the new token via a callback.

**Required scopes:**
```
offline_access files.readwrite.all user.read email openid profile
```

For read-only operation:
```
offline_access files.read files.read.all sites.read.all
```

**Critical:** Always persist the refresh token. If it is lost, the user must re-authenticate.

### 3.4 Authorization Code Flow with PKCE

Alternative to device code flow for environments with browser access.

1. Generate PKCE code verifier and S256 challenge.
2. Redirect user to authorization URL with `code_challenge` and `code_challenge_method=S256`.
3. Exchange authorization code for token with `code_verifier`.

**Important:** Manually set the token `Expiry` field from `expires_in` in the response. The standard Go `oauth2` library may not populate this correctly, which prevents automatic token refresh.

---

## 4. Rate Limiting and Throttling

### 4.1 HTTP 429 Too Many Requests

The primary throttling signal. The response includes a `Retry-After` header (in seconds).

**Handling:**
1. Read the `Retry-After` header value.
2. Wait for the specified duration.
3. Retry the request.

If no `Retry-After` header is present, use exponential backoff.

### 4.2 HTTP 503 Service Unavailable

Indicates temporary server-side issues. May include `Retry-After` header.

**Handling:** Wait 30 seconds (reference implementation default) and retry.

### 4.3 HTTP 504 Gateway Timeout

Server did not receive a timely response from an upstream server.

**Handling:** Same as 503 -- wait and retry.

### 4.4 HTTP 509 Bandwidth Limit Exceeded (SharePoint)

SharePoint-specific throttling for exceeding bandwidth caps.

**Handling:** Back off significantly and retry later.

### 4.5 HTTP 507 Insufficient Storage

Storage quota has been exceeded. This is **not retryable** -- the user must free up space.

### 4.6 Exponential Backoff Strategy

The reference implementation uses:
```
backoff = baseInterval * (2 ^ min(retryAttempts, 10))
backoff = min(backoff, maxBackoffInterval)
```

With parameters:
- Base interval: 1 second
- Maximum backoff: 120 seconds
- Maximum retry count: 175,200 (approximately 365 days of continuous retry)

**Recommended implementation for our client:**

| Attempt | Delay |
|---|---|
| 1 | 1s |
| 2 | 2s |
| 3 | 4s |
| 4 | 8s |
| 5 | 16s |
| 6 | 32s |
| 7 | 64s |
| 8+ | 120s (cap) |

### 4.7 Per-App and Per-User Limits

Microsoft Graph applies throttling at multiple levels:
- **Per-app per-user:** Each app has a limit for a specific user.
- **Per-app across all users:** Aggregate limit for the application.
- **Per-tenant:** Aggregate limit for all apps in a tenant.

Exact limits are not published by Microsoft and may vary. The practical approach is robust `Retry-After` handling.

### 4.8 Retryable vs Non-Retryable Status Codes

| Status Code | Retryable | Notes |
|---|---|---|
| 401 | Yes (once) | Token may have expired; OAuth2 transport will refresh. |
| 408 | Yes | Request timeout. |
| 429 | Yes | Rate limited; honor `Retry-After`. |
| 500 | Yes | Internal server error; transient. |
| 502 | Yes | Bad gateway; transient. |
| 503 | Yes | Service unavailable; transient. |
| 504 | Yes | Gateway timeout; transient. |
| 400 | No | Bad request. Fix the request. |
| 403 | No | Forbidden. Permission issue. |
| 404 | No | Not found. Item does not exist. |
| 409 | No | Conflict. Handle appropriately. |
| 410 | No | Gone. Delta token expired; full resync needed. |
| 412 | No | Precondition failed. eTag mismatch. |
| 413 | No | Payload too large. |
| 507 | No | Insufficient storage. |

---

## 5. Personal vs Business vs SharePoint Differences

### 5.1 Comprehensive Comparison Table

| Feature / Behavior | OneDrive Personal | OneDrive for Business | SharePoint Document Library |
|---|---|---|---|
| **Drive type string** | `personal` | `business` | `documentLibrary` |
| **QuickXorHash** | Available | Available | Available |
| **SHA1 hash** | Available | Not available | Not available |
| **SHA256 hash** | Not available | Available | Available |
| **CRC32 hash** | Sometimes available | Not available | Not available |
| **cTag on delta create/modify** | Present | Omitted | Omitted |
| **cTag on delta delete** | Omitted | Omitted | Omitted |
| **name on delta delete** | Present | Omitted | Omitted |
| **size on delta delete** | Omitted | Present | Present |
| **Simple upload max size** | 4 MB | 4 MB | 4 MB |
| **Resumable upload max file** | 250 GB | 250 GB | 250 GB |
| **permanentDelete API** | Not supported | Supported | Supported |
| **Timestamp token in delta** | Not supported | Supported | Supported |
| **`parentReference.driveId` format** | 16 hex chars (but buggy, see below) | Standard GUID | Standard GUID |
| **Shared folder sync header** | Always needed (`Include-Feature=AddToOneDrive`) | Needed only if `sync_business_shared_items` enabled | Same as Business |
| **WebSocket notifications** | Supported | Supported | Supported |

### 5.2 Hash Availability Details

For file integrity verification during sync:

| Hash Type | JSON Path | Personal | Business | SharePoint |
|---|---|---|---|---|
| QuickXorHash | `file.hashes.quickXorHash` | Yes | Yes | Yes |
| SHA1 | `file.hashes.sha1Hash` | Yes | No | No |
| SHA256 | `file.hashes.sha256Hash` | No | Yes | Yes |
| CRC32 | `file.hashes.crc32Hash` | Sometimes | No | No |

**Recommendation:** Use QuickXorHash as the primary integrity check since it is available across all account types. Implement QuickXorHash computation locally for verification.

**Edge case:** Some items (e.g., zero-byte files, certain system files) may have a QuickXorHash of `AAAAAAAAAAAAAAAAAAAAAAAAAAA=` (all zeros). Treat this as a valid hash, not a missing hash.

### 5.3 cTag Behavior Differences

The `cTag` (content tag) changes only when file content changes, unlike `eTag` which changes on any metadata change.

- **Personal:** `cTag` is reliably present on non-deleted items.
- **Business/SharePoint:** `cTag` is often omitted in delta responses for created/modified items. This means the sync engine cannot rely on `cTag` for change detection in Business accounts.
- **All types:** `cTag` is omitted for deleted items.

### 5.4 Personal Account `parentReference.driveId` Bug

The reference implementation documents a known API bug (Issue #3072): On Personal accounts, the `parentReference.driveId` value may not be the expected 16-character hexadecimal string. It may be shorter or have inconsistent casing.

**Workaround:**
- Validate the driveId is 16 characters long.
- If shorter, pad with leading zeros.
- If the validated `appConfig.defaultDriveId` is known, use it as a fallback.
- Force driveId values to lowercase for consistent comparison.

### 5.5 Shared Folder Differences

- **Personal:** Shared folders appear as `remoteItem` facets in the delta response. The `Include-Feature=AddToOneDrive` header is required to receive them.
- **Business:** Shared folders require explicit opt-in via configuration. The same `Include-Feature=AddToOneDrive` header is used.
- **SharePoint:** SharePoint links and shared document libraries are discovered via the sites API (`/sites?search=*` and `/sites/{siteId}/drives`).

### 5.6 SharePoint-Specific Considerations

- SharePoint document libraries may have different file size values during download (the API may not report accurate sizes).
- SharePoint files containing metadata may have size differences between upload and what the API reports (metadata overhead).
- Quota information may be restricted or unavailable on SharePoint and Business shared folders.

---

## 6. Edge Cases and Gotchas

### 6.1 `@microsoft.graph.downloadUrl` Expiry

The pre-authenticated download URL returned in item metadata expires after approximately **1 hour**. The sync engine must:
- Never cache this URL between sync cycles.
- Re-fetch item metadata if the URL has expired.
- Handle `403 Forbidden` or `401 Unauthorized` as potential indicators of URL expiry.

### 6.2 `@microsoft.graph.conflictBehavior` Placement

This parameter is used in different locations depending on the operation:

| Operation | Placement |
|---|---|
| Folder creation (POST) | Request body: `"@microsoft.graph.conflictBehavior": "rename"` |
| Upload session creation | Request body within `"item"`: `{ "item": { "@microsoft.graph.conflictBehavior": "rename" } }` |
| Upload error recovery (PUT with sourceUrl) | Request body: `"@microsoft.graph.conflictBehavior": "rename"` |

### 6.3 `parentReference.path` Not in Delta

As documented in the official API: the `parentReference` property on items in delta responses **does not include the `path` value**. This is because renaming a folder does not cause descendants to reappear in delta results.

**Impact:** The sync engine must maintain a local database mapping item IDs to paths, reconstructing paths by walking the parent chain. Items must be tracked by `id`, never by path alone.

### 6.4 Deleted Item Property Omissions

On Business accounts, deleted items in delta responses may lack the `name` property. On Personal accounts, deleted items lack `size`. The sync engine must:
- Use item `id` as the primary identifier for deletion operations.
- Look up item details from the local database when the delta response omits properties.

### 6.5 `fileSystemInfo` Missing on Remote Items

Items from "Add Shortcut" (remote items / shared folder shortcuts added to a user's drive) may lack the `fileSystemInfo` facet. The sync engine should:
- Check for `fileSystemInfo` existence before accessing timestamps.
- Fall back to `createdDateTime` / `lastModifiedDateTime` at the DriveItem level.
- For remote items, check `remoteItem.fileSystemInfo` as well.

### 6.6 Invalid Timestamps

The API may return timestamps that are clearly invalid (e.g., dates in the far past like `0001-01-01T00:00:00Z` or far future). The sync engine should:
- Validate timestamps before using them for local file operations.
- Treat zero-value or clearly invalid timestamps as "unknown" and use the current time or server-reported modification time.

### 6.7 Large File Upload Session eTag Changes

When creating an upload session with `If-Match` eTag, the server may change the eTag during session creation. Subsequent fragment uploads with the original eTag cause:
1. `412 Precondition Failed`
2. `416 Requested Range Not Satisfiable`

**Workaround:** Do not use `If-Match` headers on upload session creation. Instead, verify the file state after upload completes by comparing the response DriveItem metadata.

### 6.8 Fragment Upload Authentication

The `uploadUrl` returned by `createUploadSession` is **pre-authenticated**. Sending an `Authorization` header with fragment PUT requests can cause `401 Unauthorized` errors. Only use the `Authorization` header for the initial `createUploadSession` POST.

### 6.9 Fragment Size Requirements

Each upload fragment **must** be a multiple of 320 KiB (327,680 bytes), except for the final fragment. Using non-compliant sizes can cause upload failures even after all bytes are uploaded. The reference implementation uses a chunk size of `320 * 1024 * 4` = 1.25 MB (1,310,720 bytes = 4 fragments of 320 KiB).

### 6.10 Quota Exceeded Behavior

| Status Code | Meaning |
|---|---|
| `507 Insufficient Storage` | Upload or session creation fails due to quota. |
| `413 Payload Too Large` | Individual request is too large. |

When quota is exceeded:
- `createUploadSession` returns `507` immediately if `fileSize` is specified and exceeds available quota.
- Simple uploads fail with `507`.
- The `Drive.quota.state` field transitions through: `normal` -> `nearing` -> `critical` -> `exceeded`.

### 6.11 HTTP/2 Status Code 0

When using HTTP/2, some platforms (notably older versions of curl on Ubuntu) may return status code `0` instead of `200 OK`. The sync engine should treat status code `0` as equivalent to `200 OK` when HTTP/2 is in use.

### 6.12 Reserved and Invalid Filenames

OneDrive enforces filename restrictions:

**Invalid characters:** `< > : " / \ | ? *`

**Reserved names (Windows):** `CON`, `PRN`, `AUX`, `NUL`, `COM1`-`COM9`, `LPT1`-`LPT9` (case-insensitive, with or without extensions).

**Other restrictions:**
- Filename cannot end with a period `.` or space ` `.
- Maximum filename length: 255 characters.
- Maximum path length: 400 characters.

### 6.13 Empty Delta Responses After All Items Processed

If a delta call returns items AND a `@odata.deltaLink` in the same response (no `@odata.nextLink`), this is the final page. If a subsequent incremental delta call returns `{ "value": [], "@odata.deltaLink": "..." }` with an empty value array, this simply means no changes have occurred since the last sync.

### 6.14 Root Item Identification

Root items in delta responses have a `root` facet: `"root": {}`. They may also have unusual `parentReference` structures. The sync engine should use the `root` facet, not the `parentReference` structure, to identify root items.

Some shared folder root items on SharePoint may have `parentReference.driveType` of `"documentLibrary"` while the item itself has a `name` equal to the shared folder's display name.

### 6.15 Download URL `AVOverride` Parameter

The reference implementation appends `?AVOverride=1` to download content URLs. This parameter appears to bypass server-side antivirus scanning delays. While undocumented, it may improve download performance for files already known to be safe.

### 6.16 Package Items (OneNote)

Items with a `package` facet (typically OneNote notebooks with `"type": "oneNote"`) should be treated specially:
- They appear as folders in the API but should not be synced as regular folders.
- Their internal structure is managed by the OneNote service.
- The sync engine should skip or handle these based on configuration.

### 6.17 Concurrent Modification During Sync

If the user modifies a file locally while the sync engine is uploading it, or if the server-side file changes during download:
- Use `eTag` and `If-Match` headers for optimistic concurrency control.
- Verify file integrity after download using QuickXorHash.
- For uploads, compare the returned DriveItem metadata against the local file state.

### 6.18 Socket.IO WebSocket Notifications

The WebSocket endpoint provides low-latency change notifications but:
- Requires libcurl 7.86.0+ for WebSocket support.
- The notification only signals that something changed; the sync engine must still call the delta API to get the actual changes.
- WebSocket connections may drop and require reconnection logic.
- Not available as a standalone change detection mechanism -- always use with delta polling as a fallback.

---

## Appendix A: DriveItem Data Model (Sync-Relevant Fields)

```
DriveItem {
  id                    string          -- Unique identifier; primary key for sync
  name                  string          -- Display name; may be omitted for deleted items (Business)
  size                  int64           -- Size in bytes; may be omitted for deleted items (Personal)
  eTag                  string          -- Changes on any metadata update
  cTag                  string          -- Changes only on content updates; often omitted (see section 5.3)
  createdDateTime       time.Time
  lastModifiedDateTime  time.Time
  webUrl                string

  parentReference {
    driveId             string          -- ID of parent drive
    driveType           string          -- "personal", "business", "documentLibrary"
    id                  string          -- ID of parent folder
    path                string          -- NOT returned in delta responses
  }

  fileSystemInfo {
    createdDateTime     time.Time       -- Client-reported creation time
    lastModifiedDateTime time.Time      -- Client-reported modification time
  }

  file {
    mimeType            string
    hashes {
      sha1Hash          string          -- Personal only
      sha256Hash        string          -- Business/SharePoint only
      quickXorHash      string          -- All account types
      crc32Hash         string          -- Sometimes on Personal
    }
  }

  folder {
    childCount          int
  }

  deleted {
    state               string          -- Presence indicates item is deleted
  }

  remoteItem {
    id                  string          -- ID on the remote drive
    name                string
    size                int64
    fileSystemInfo      {...}           -- May be null for "Add Shortcut" items
    folder              {...}
    file                {...}
  }

  package {
    type                string          -- e.g., "oneNote"
  }

  root                  {}              -- Presence indicates this is a root item

  @microsoft.graph.downloadUrl  string  -- Pre-authenticated download URL (~1 hour TTL)
}
```

---

## Appendix B: OAuth2 Scopes Reference

| Scope | Purpose |
|---|---|
| `offline_access` | Required for refresh token. |
| `files.readwrite.all` | Read/write access to all files. |
| `files.read` | Read-only access to user's files. |
| `files.read.all` | Read-only access to all files. |
| `sites.readwrite.all` | Read/write access to SharePoint sites. |
| `user.read` | Read user profile. |
| `email` | Read user's email address. |
| `openid` | OpenID Connect sign-in. |
| `profile` | Read user's basic profile. |

---

## Appendix C: Quick Reference -- All Endpoint URLs

### Core Sync Operations

| Operation | Method | URL Pattern |
|---|---|---|
| Delta (initial) | GET | `/drives/{driveId}/root/delta` |
| Delta (incremental) | GET | `{@odata.deltaLink}` (full URL) |
| Delta (latest token) | GET | `/drives/{driveId}/root/delta?token=latest` |
| Get item by ID | GET | `/drives/{driveId}/items/{itemId}` |
| Get item by path | GET | `/me/drive/root:/{path}:` |
| List children | GET | `/drives/{driveId}/items/{itemId}/children` |
| Simple upload | PUT | `/drives/{driveId}/items/{parentId}:/{filename}:/content` |
| Create upload session | POST | `/drives/{driveId}/items/{parentId}:/{filename}:/createUploadSession` |
| Upload fragment | PUT | `{uploadUrl}` (pre-authenticated) |
| Download by ID | GET | `/drives/{driveId}/items/{itemId}/content` |
| Create folder | POST | `/drives/{driveId}/items/{parentId}/children` |
| Update/Rename/Move | PATCH | `/drives/{driveId}/items/{itemId}` |
| Delete | DELETE | `/drives/{driveId}/items/{itemId}` |
| Copy (async) | POST | `/drives/{driveId}/items/{itemId}/copy` |

### Supporting Operations

| Operation | Method | URL Pattern |
|---|---|---|
| Get default drive | GET | `/me/drive` |
| Get drive by ID | GET | `/drives/{driveId}` |
| List drives | GET | `/me/drives` |
| Search | GET | `/drives/{driveId}/root/search(q='{query}')` |
| Special folder | GET | `/me/drive/special/{name}` |
| Shared with me | GET | `/me/drive/sharedWithMe` |
| User profile | GET | `/me` |
| Create subscription | POST | `/v1.0/subscriptions` |
| WebSocket endpoint | GET | `/v1.0/drives/{driveId}/root/subscriptions/socketIo` |

### Authentication

| Operation | Method | URL Pattern |
|---|---|---|
| Device code request | POST | `https://login.microsoftonline.com/{tenant}/oauth2/v2.0/devicecode` |
| Token exchange/refresh | POST | `https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token` |
| Authorization | GET | `https://login.microsoftonline.com/{tenant}/oauth2/v2.0/authorize` |
