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

Some Business/SharePoint files have no hash values at all. Microsoft confirmed this is a known issue: for enumeration/delta scenarios, hash generation may be "too expensive" for certain files. Zero-byte files consistently have no hash.

Runtime policy:
- the Graph boundary preserves the file's size, mtime, and eTag even when every hash field is empty
- the sync baseline persists that metadata per side instead of collapsing it into one shared file record
- remote equality falls back to `size + mtime + eTag` only when both remote-side hashes are absent
- local equality falls back to `size + mtime` only when both local-side hashes are absent
- missing hashes are never equality by themselves; unknown metadata stays conservative

### iOS HEIC File Metadata Mismatch

`.heic` files uploaded from iOS via the OneDrive app have metadata (size, hash) that doesn't match the actual download content. The API reports the original file size but serves a post-processed (smaller) version. Example: API reports 3,039,814 bytes, actual download is 682,474 bytes. Filed as [onedrive-api-docs#1723](https://github.com/OneDrive/onedrive-api-docs/issues/1723), unresolved.

### SharePoint File Enrichment

SharePoint injects metadata into uploaded files, changing the hash and size post-upload. This can cause infinite upload/download loops if upload validation compares against the original local file. Filed as [onedrive-api-docs#935](https://github.com/OneDrive/onedrive-api-docs/issues/935).

### Upload Creates Double Versions (Business/SharePoint)

On Business/SharePoint, uploading a file and then PATCHing its `fileSystemInfo` (to preserve local timestamps) creates two versions, doubling storage consumption. The fix is to include `fileSystemInfo` in the upload session creation request, setting timestamps atomically with the content. Modified file re-uploads still create an extra version (unfixed API bug, [onedrive-api-docs#877](https://github.com/OneDrive/onedrive-api-docs/issues/877)).

### Simple Upload Cannot Set Metadata

Simple upload (PUT `/content`) sends raw binary — there's no way to include `fileSystemInfo` in the same request. Files ≤4 MiB uploaded via simple upload get server receipt timestamps. A post-upload PATCH to `UpdateFileSystemInfo` is required when mtime preservation matters.

## Delta Response Quirks

## Socket.IO Watch Endpoint Shape

### Graph Returns A Pre-Auth Notification URL, Not A Ready-To-Dial WebSocket URL

`GET /drives/{driveID}/root/subscriptions/socketIo` returns a sensitive
pre-authenticated `notificationUrl`. The client cannot dial that value as-is.
Runtime policy:

- treat `notificationUrl` as sensitive credential material and redact it from logs/errors
- transform it to `/socket.io/?EIO=4&transport=websocket` on the same host/query before dialing
- preserve Graph-supplied query parameters during that transform

### Engine.IO Open Precedes Namespace Join

After the websocket connect succeeds, the server first sends an Engine.IO open
packet (`0{...}` with a Socket.IO session ID). Only after receiving that open
packet should the client send Socket.IO namespace connect frames (`40` and
`40/notifications`).

### Ping/Pong Is Transport Liveness Only

Engine.IO ping (`2`) and pong (`3`) frames are transport keepalive, not change
data. Missing or malformed ping/pong handling drops the session but must not
mutate delta-token state.

### `notification` Events Are Wake Signals Only

Socket.IO `notification` events do not carry authoritative remote item state.
They are only a low-latency wakeup telling the client to run the ordinary
delta pipeline. The delta feed remains the sole source of remote truth.

### Socket.IO Notification Latency Is Variable

Live `e2e_full` coverage on April 5, 2026 showed that a websocket watch
session can connect immediately and still receive the first
`42/notifications,["notification", ...]` packet tens of seconds after the
remote mutation. In the same account and drive, captured successful runs
delivered the first notification roughly 19s, 32s, and 36s after the socket
handshake. Another run kept the session connected but produced no notification
within a 45s test window before the test aborted.

Runtime policy:
- treat Socket.IO as a low-latency wake path, not a hard real-time guarantee
- keep ordinary delta polling as the authoritative fallback
- full E2E websocket tests should allow a wider notification window and assert
  the end-to-end file arrival stays well below the 5-minute polling fallback,
  rather than assuming every notification arrives inside 45s

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

### Folder-Scoped Delta Readiness Lag

On Personal drives, a newly created folder can become path-resolvable before
its folder-scoped delta feed is ready. A direct `GET /drives/{driveID}/root:/path:`
can return `200` for the folder while the immediate first
`GET /drives/{driveID}/items/{folderID}/delta` still returns HTTP 404
`itemNotFound`. This showed up in the fast E2E lane on April 6, 2026 for a
fresh `sync_paths` bootstrap: the selected folder existed and contained files,
but initial scoped delta still lagged the folder lookup path.

Runtime policy:
- treat this as a Graph readiness quirk, not as proof the configured path is invalid
- when a personal `sync_paths` scoped bootstrap already resolved the folder by
  path but folder-scoped delta still fails, fall back to recursive subtree
  enumeration for that scope instead of failing the whole sync pass
- keep retrying scoped delta on later passes once the folder feed becomes ready

### Ephemeral Deletion Events

Delta deletion events are delivered **exactly once**. If the client's token window advances past a deletion (including via a zero-event response that returns a new token), the deletion is permanently missed. A subsequent incremental delta call will never report that deletion again. Fresh delta (no token) enumerates only existing items — it never reports deletions. This means incremental delta is the **only** way to learn about deletions, and it has exactly one chance to deliver each one.

The combination of consistency lag (above) and ephemeral deletions creates a window where: (1) a deletion is performed, (2) the client calls delta before the deletion propagates to the change log, (3) delta returns zero events + new token, (4) the client saves the new token, permanently skipping the deletion.

Mitigation requires both a zero-event token guard (don't advance token on empty responses) and periodic full reconciliation (enumerate all items, detect orphans in baseline).

### Personal Phantom System Drives

Every Personal account has 2-3 hidden system drives (face crops, albums) created by Microsoft for the Photos app. They report `driveType: "personal"` and share quota numbers with the real OneDrive, but return HTTP 400 `ObjectHandle is Invalid` when accessed. `GET /me/drive` (singular) returns the real drive. `GET /me/drives` (plural) returns all drives in non-deterministic order.

Runtime policy:
- Authentication/bootstrap uses `GET /me/drive` directly for the primary drive.
- General drive discovery still starts with `GET /me/drives` so business and document-library entries are preserved, but any returned personal entries are replaced with the single authoritative `GET /me/drive` result before the list reaches callers.
- The client does not currently synthesize a missing personal drive if `/me/drives` omits it entirely; that would require repo-local evidence before adding another normalization rule.

## HTTP Protocol Violations

### Transient 404 on Valid Resources

`GET /drives/{driveID}/items/root/children` returns HTTP 404 with Graph code `itemNotFound` on drives that have existed for months. The token is valid, the drive ID is correct, and subsequent requests succeed. Caused by cross-datacenter lookup timeouts on the load balancer. This is a server-side failure misreported as a client error. Frequency: ~1% of requests.

### Transient 404 on Fresh Download Metadata by Item ID

After a file is newly created, `GET /drives/{driveID}/items/{itemID}` can
return HTTP 404 with Graph code `itemNotFound` even though the same file is
already visible through delta and path-based lookup. This shows up most
clearly in download flows: sync sees the new item in delta, then the immediate
item-by-ID metadata fetch used to obtain `@microsoft.graph.downloadUrl` still
fails briefly.

Observed local evidence on April 5, 2026 during `go run ./cmd/devtool verify default`:

- `mkdir` succeeded for `/e2e-sync-dl-1775446530960515000`
- `put` succeeded for `/e2e-sync-dl-1775446530960515000/download-test.txt`
- `stat /e2e-sync-dl-1775446530960515000/download-test.txt` succeeded
- the following `sync --download-only --force` pass observed the file in delta
- the worker then got `GET /drives/bd50cf43646e28e6/items/BD50CF43646E28E6!sccc09df70b5f4290916bf66a63006a39 = 404 itemNotFound`
  with request ID `67e72d16-ea0c-4c25-aa82-3ece389a8c8e`

Runtime policy:

- `graph.Client.Download()` and `DownloadRange()` own the exact quirk retry
- retry remains narrow: only item-by-ID metadata fetches used to obtain the
  pre-authenticated download URL, only HTTP 404, only Graph code
  `itemNotFound`
- the current production retry budget is 4 total attempts with 250ms base, 2x
  multiplier, no jitter, and 2s max
- transfer-manager callers and sync workers do not add their own second retry
  loop for this quirk; the graph boundary is the single owner

### Post-Mutation Path Reads Can Lag Successful `mkdir`, `put`, And `mv`

Graph can acknowledge a metadata-changing mutation and still return
`404 itemNotFound` on an immediate follow-on path lookup for the destination.
This is a different consistency gap from the item-by-ID download metadata
misfire above: the mutation itself succeeded, but the path-based read model has
not converged yet.

Observed evidence on April 5, 2026 during `go run ./cmd/devtool verify
e2e-full`:

- `put /e2e-sync-ee-1775448127403708000/conflict-file.txt` completed after the
  remote edit step of `TestE2E_Sync_EditEditConflict_ResolveKeepRemote`
- the immediate follow-on `stat /e2e-sync-ee-1775448127403708000/conflict-file.txt`
  kept returning HTTP 404 `itemNotFound` with request ID
  `0d76a7d9-c2b4-4eec-acd2-de29e5aec5c7` until the test's 30s poll window
  expired

Runtime policy:

- CLI mutation commands own a bounded post-success visibility wait
- `mkdir`, single-file `put`, and `mv` retry destination-path reads on exact
  `itemNotFound` using a short deterministic schedule: `250ms`, `500ms`, `1s`,
  `2s`, `4s`, `8s`, `16s`
- if the destination path still is not readable after that budget, the command
  returns `ErrPathNotVisible` instead of claiming success prematurely

Observed extension trigger on April 5, 2026:

- `put /e2e-sync-bidi-1775450238168612000/data/info.txt` in
  `TestE2E_Sync_BidirectionalMerge` succeeded at the upload/PATCH layer, but
  the immediate post-success path check still got 7 consecutive `GET by path =
  404 itemNotFound` responses under the older `250ms, 500ms, 1s, 2s, 4s, 8s`
  schedule
- request IDs from that failed old-budget run:
  `7664d8e9-3236-43bb-bd7f-10a1cb0bb877`,
  `3f7b9699-68b1-4401-805f-c1cc26ba9c2a`,
  `ee928458-b335-4fe7-be53-425f4c9b6ab6`,
  `5200b5b8-7236-46fc-87d8-9f2a4cd2b60d`,
  `346757be-67cf-4363-bfa1-aedf0f51f634`,
  `c4e63564-7be3-4e33-b62d-0633d13ee42f`,
  `0d54bce7-4842-4831-b246-617223a0fe71`
- the production schedule was therefore extended to include the final `16s`
  step before surfacing `ErrPathNotVisible`

### Delete-By-ID Can Lag A Successful Path Lookup

Graph can also disagree with itself in the opposite direction: a path lookup
can succeed, then the immediate delete by that item ID can still return
`404 itemNotFound`. The user-facing intent for `rm` / forced overwrite is the
path, not the first resolved ID, so the CLI cannot fail immediately on that
delete-route inconsistency.

Observed evidence on April 5, 2026 during `go test -tags='e2e e2e_full' ./e2e
-run '^TestE2E_EdgeCases$|^TestE2E_Sync_BidirectionalMerge$'`:

- cleanup for `TestE2E_EdgeCases/concurrent_uploads` resolved
  `/onedrive-go-e2e-edge-1775450932112095000/concurrent-1.txt` successfully by
  path
- the immediate `DELETE /drives/bd50cf43646e28e6/items/BD50CF43646E28E6!s5fd8d410d21d4234a567f45e01fc46e2`
  still returned HTTP 404 `itemNotFound`
- Graph request ID for the failed delete: `335ea56d-e3a9-4d2f-8b4c-742da9088eec`

Runtime policy:

- CLI path-oriented deletes use path-authoritative recovery, not one-shot
  item-ID authority
- `rm`, `mv --force`, and `cp --force` delete through
  `driveops.Session.DeleteResolvedPath()` / `PermanentDeleteResolvedPath()`
- when the initial delete returns exact `404 itemNotFound`, the session:
  - re-resolves the target path
  - accepts a now-missing path as success
  - retries delete against the current resolved item when the path still exists
  - uses the same bounded deterministic convergence schedule as
    post-success path visibility
- `rm` additionally waits for the non-root parent path to become readable again
  before printing success

### Transient 403 on `/me/drives` (Token Propagation)

After token refresh, `/me/drives` returns HTTP 403 with Graph code `accessDenied` while `/me` (user profile) succeeds with the same token. Caused by eventual consistency in Microsoft's token propagation infrastructure. The `/me` endpoint receives token updates before `/me/drives`.

Observed CI evidence:

- scheduled `e2e_full` run on April 5, 2026 (`23999446320`) got `GET /me = 200`
- the same test then got 3 consecutive `/me/drives` failures over about 3.1
  seconds under the old production retry budget
- a later `whoami` command in the same run succeeded again roughly 10-12
  seconds later, so the failure window was transient rather than a durable
  auth rejection

Runtime policy:

- `graph.Client.Drives()` owns the exact quirk retry
- retry remains narrow: only `/me/drives`, only HTTP 403, only Graph code
  `accessDenied`
- the current production retry budget is 5 total attempts with 1s base, 2x
  multiplier, ±25% jitter, and 60s cap
- after that budget is exhausted, CLI discovery surfaces degrade explicitly
  instead of misclassifying the account as auth-required:
  - `whoami` keeps the authenticated user and falls back to `/me/drive` when
    possible
  - `drive list` keeps configured/offline state, uses `/me/drive` fallback for
    the primary drive, and continues independent shared-folder and SharePoint
    discovery

### Slow/Stalled Metadata Response Headers

Ordinary Graph metadata requests can sometimes connect successfully and then
stall for tens of seconds before sending response headers. This was observed
in the scheduled `e2e_full` CI run on April 3, 2026 during setup for the
big-delete protection test: a normal metadata/path-resolution request hung
long enough that a client-wide 30-second timeout misclassified it as canceled
work even though later attempts succeeded. The same family showed up again in
GitHub Actions integration on April 5, 2026: a plain `GET /me` request stalled
for roughly 60 seconds before the caller budget expired, while the rest of the
live Graph suite completed normally.

Runtime policy:

- Graph-facing metadata clients do not use `http.Client.Timeout`
- metadata transports use connection-level deadlines such as
  `ResponseHeaderTimeout` so a stalled attempt returns a transport timeout
  rather than canceling the caller context
- interactive CLI metadata requests may retry that transport failure
- sync metadata requests remain single-attempt and return the failure to the
  engine for normal classification

This is documented as a transport-shape problem, not as evidence for a new
Graph semantic retry rule.

### No General Ordinary-Request Post-Refresh 401 Quirk (Current Evidence)

The repository does **not** currently have captured evidence for a general
"ordinary Graph request returns a spurious 401 after the client already
refreshed the saved login and retried once" quirk. Because that evidence is
absent, runtime handling remains narrow:

- ordinary authenticated requests refresh and retry once on 401
- if the retried request still returns 401, the graph boundary returns
  `ErrUnauthorized`
- sync treats that as fatal for the current pass/watch and does not trial or
  back off 401

If recoverable payload evidence or a reproducible ordinary-request post-refresh
401 incident is captured later, targeted graph-boundary handling can be added
then.

### HTTP 308 Without Location Header

Webhook subscription renewal (PATCH to `/v1.0/subscriptions/{id}`) returns HTTP 308 (Permanent Redirect) without a `Location` header, violating RFC 7538. Observed on Personal accounts migrated to the new backend, specifically in the West Europe data center. Filed as [onedrive-api-docs#1895](https://github.com/OneDrive/onedrive-api-docs/issues/1895).

### Personal Async Copy And Upload Hosts

Personal-account pre-auth URLs can use `*.microsoftpersonalcontent.com` rather than Graph or SharePoint hosts. This is now observed on both async-copy monitor URLs and upload-session URLs. The client treats this host family as trusted for those pre-auth flows only. This host is still not trusted for Graph base URLs.

### Historical HTTP 400 "ObjectHandle is Invalid" Incident

One multi-hour Microsoft-side backend incident was observed returning HTTP 400 with message text such as `"ObjectHandle is Invalid"` across Graph API calls. The repository no longer has recoverable CI payload evidence for the exact body shape, so the runtime does **not** implement a special classifier for this incident. If the behavior reappears and CI captures the payload, targeted handling can be reconsidered. Observed: one 3.5-hour incident.

### HTTP 507 Wraps ErrServerError

HTTP 507 (Insufficient Storage) wraps `ErrServerError` at the sentinel level. If error classification checks sentinels before status codes, 507 is incorrectly classified as retryable (like other 5xx errors). Status code classification must take priority.

## Throttling Scopes

Official Microsoft docs describe throttling as multi-bucket: a request may be
checked against user, app-per-tenant, tenant, and other service-specific
limits, and the first limit reached returns HTTP 429. Ordinary OneDrive file
traffic does not give this repository a stable, files-specific signal that says
which broad server-side bucket fired for a given request.

Runtime policy therefore stays intentionally narrow:

- `Retry-After` on 429/503 is authoritative and must be honored exactly
- client-side blocking uses the narrowest remote boundary provable from the
  failed request
- own-drive file traffic blocks only that drive
- shared-folder or direct shared-item traffic block only that exact shared
  root/item boundary
- broader account-wide or tenant-wide blocking is **not** inferred from an
  ordinary file 429 without explicit evidence

This is a deliberate client-policy choice, not a claim that Microsoft's server
limits are drive-local. The service may still enforce broader quotas. The
client simply avoids proactively suppressing unrelated work when the response
does not identify a broader bucket with enough confidence.

`RateLimit-Remaining` and `RateLimit-Limit` headers exist but only cover
per-app 1-minute resource units. They are not reliable enough for proactive
throttling decisions across file operations.

## Shared Folder Issues

### External Shared Discovery Is Best-Effort, Not Guaranteed

Microsoft Graph shared discovery remains incomplete for some external and
cross-organization shares even on the supported search surface. A successful
live search is not proof that every share is currently visible.

Runtime policy:
- shared discovery uses `GET /me/drive/root/search(q='*')` only
- actionable results must expose both `remoteDriveID` and `remoteItemID`
- if live search succeeds but still omits a shared folder, the CLI explains
  that Graph omitted the item and points the user at `shared` / `drive list`
  to confirm what the API exposed
- if live search itself fails for an authenticated account, callers surface a
  degraded discovery notice instead of silently omitting the account

This is a documented platform-limit explanation, not evidence that all
cross-org shares are categorically invisible.

### Permissions Endpoint Mixes Caller Grants With Owner Rows

`GET /drives/{driveID}/items/{itemID}/permissions` on shared folders does not
behave like a pure "current caller effective permissions" endpoint. Live
Personal-account testing on a read-only shared folder returned:

- an anonymous/view link grant for the recipient's actual access path
- a separate owner membership row for the sharer with `roles: ["owner"]`
- a bare `link.webUrl` on that owner row even though it was not itself the
  caller's effective share grant

If the client treats every returned permission row as caller-applicable, the
owner row falsely proves the folder is writable and read-only shared folders
are misclassified as transient 403s.

Runtime policy:
- evaluate permission applicability per row, not just `roles`
- treat link grants as caller-applicable only when the link facet actually
  carries grant semantics (`type` and/or `scope`), not merely a `webUrl`
- ignore owner or direct-grant rows that explicitly target some other user
- fail open when the remaining evidence is ambiguous rather than suppressing
  writes on stale or misattributed permission data

### Share Links Resolve To Underlying Item Identity, Not Original Discovery Links

`/shares/{shareIdOrEncodedSharingUrl}/driveItem` resolves a sharing grant to the
underlying owner-side item identity (`driveId` + `itemId`). Shared-item search
does not reliably return the original email/text share URL that was sent to the
recipient, and `driveItem.webUrl` is not a substitute for that original inbound
sharing link.

Runtime policy:
- raw share URLs are accepted as input aliases and normalized immediately to
  the underlying shared item identity
- CLI discovery prints generated `shared:<recipientEmail>:<remoteDriveID>:<remoteItemID>`
  selectors, not original inbound links
- sharing-link identity is treated as grant/provenance; item identity is the
  canonical operation target

### Simple Upload Misreports Read-Only Shared Folder Creates

On OneDrive Personal shared folders, creating a new child file with simple
upload (`PUT /drives/{driveID}/items/{folderID}:/{name}:/content`) can return
HTTP 404 `itemNotFound` on a real read-only shared folder even though the
folder itself is readable and listable. The equivalent upload-session creation
request (`POST ...:/createUploadSession`) against the same folder returns the
correct HTTP 403 `accessDenied`.

Runtime policy:
- simple upload remains the fast path for small creates
- if that create-by-parent simple upload returns `ErrNotFound` for a non-zero
  file, retry once via `createUploadSession`
- the session route becomes the authoritative result for permission handling,
  so read-only shared folders surface `403` and enter the normal `perm:remote`
  flow instead of being misclassified as missing

### Fresh Parent Folder Can Still Reject `createUploadSession`

Live `e2e_full` coverage on April 5, 2026 captured a second create-path
consistency gap after folder creation. The parent folder
`/e2e-watch-websocket-1775447250013596000` was already readable by path lookup
(`request-id: c19f75f0-9a85-43e0-8144-0b4be7387226`), but the immediate follow-on
upload-session creation for `first.txt`
(`POST ...:/createUploadSession`, `request-id: d02b9317-d3d5-44ad-a30c-327df8c859d3`)
still returned HTTP 404 `itemNotFound`.

Runtime policy:
- `CreateUploadSession()` retries that exact fresh-parent `404 itemNotFound`
  case with a short bounded policy (`6` total attempts, `250ms` base, `4s` max,
  no jitter)
- the retry stays at the graph quirk boundary instead of teaching callers or
  generic transport retry about parent-creation ordering
- if the request still fails after the quirk budget, the upload returns the
  final error without pretending the parent exists

### Shared-Item Identity Response Shape

Graph shared-item identity is most reliably present under `remoteItem.shared`
and `remoteItem.createdBy`, not top-level `shared`. Four-level fallback chain:
`remoteItem.shared.sharedBy` → `.owner` → `remoteItem.createdBy` →
top-level `shared.owner`. The `email` field in identity responses is
undocumented but works on both personal and business accounts.

### SharedWithMe API Deprecation

`/me/drive/sharedWithMe` and `/me/drive/recent` are deprecated (November 2026
EOL). The client no longer uses `sharedWithMe`. Supported discovery uses
`GET /me/drive/root/search(q='*')`, which can return shared items with a
`remoteItem` facet but less identity data. Enrich actionable results via
`GET /drives/{driveId}/items/{itemId}`. `/me/drives` is NOT deprecated.

Observed runtime quirk on Personal accounts: `search(q='*')` can return
success with ordinary drive items but no usable shared-item identities. Runtime
policy: keep search as a best-effort surface, never fabricate selectors for
hits without `remoteDriveID` + `remoteItemID`, and degrade the account only
when the live search request itself fails.

### Cross-Organization Shared Discovery Remains Partial (Business)

The stale blanket claim that business cross-organization shares are
categorically invisible is too strong, but there is still no proof that Graph
reliably exposes every external shared folder in every tenant configuration.

Runtime policy:
- treat external shared discovery as best-effort, not guaranteed
- use `GET /me/drive/root/search(q='*')` as the only runtime discovery surface
- if search still omits the share, explain the platform limitation rather than
  claiming the folder is absent or misconfiguring the account

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

The API can return `0001-01-01T00:00:00Z` or far-future dates. `lastModifiedDateTime` may be absent or explicitly `null` for API-initiated deletions.

The client treats these as unknown metadata, not as clock values to repair locally. The graph boundary leaves invalid or absent timestamps as zero `time.Time`, sync observation/store persist that as unknown/NULL state, and CLI JSON/text output renders the unknown state explicitly instead of inventing a replacement timestamp.

### URL-Encoded Paths

`parentReference.path` (when present in non-delta responses) contains URL-encoded characters (`%20` for spaces). The client decodes it at the Graph boundary and stores the root-relative result on `graph.Item.ParentPath`. Malformed encodings or missing `/root:` markers are ignored rather than propagated as bogus paths.

## Case Sensitivity

### OneDrive Is Case-Insensitive

Two files differing only in case cannot coexist in the same OneDrive folder. The API's path-based queries perform case-insensitive matching but can return items from incorrect paths (observed with git repository files where `v1.0.0` and `v2.0.0` produce false matches). `GetItemByPath` mitigates this by post-validating the returned item against the requested path: exact reconstructed path when `parentReference.path` is present, otherwise leaf-name match only. The leaf-only path is intentionally treated as a best-effort fallback, not as exact identity.

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
