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

Runtime policy: keep the normal download integrity path (retry hash mismatch,
warn, then accept after exhaustion so the sync loop does not thrash), and make
the acceptance warning explicit for `.heic` so the degraded behavior is owned
as a known OneDrive/iOS metadata bug rather than an unexplained bypass.

### SharePoint File Enrichment

SharePoint injects metadata into uploaded files, changing the hash and size post-upload. This can cause infinite upload/download loops if upload validation compares against the original local file. Filed as [onedrive-api-docs#935](https://github.com/OneDrive/onedrive-api-docs/issues/935).

### Upload Creates Double Versions (Business/SharePoint)

On Business/SharePoint, uploading a file and then PATCHing its `fileSystemInfo` (to preserve local timestamps) creates two versions, doubling storage consumption. The fix is to include `fileSystemInfo` in the upload session creation request, setting timestamps atomically with the content. Modified file re-uploads still create an extra version (unfixed API bug, [onedrive-api-docs#877](https://github.com/OneDrive/onedrive-api-docs/issues/877)).

Runtime policy: avoid the avoidable double-version case by putting
`fileSystemInfo` into upload-session creation, accept the remaining modified
re-upload extra version as a server bug, and rely on per-side baseline hashes
to prevent re-upload/download loops instead of attempting version-history
cleanup workarounds.

### Simple Upload Cannot Set Metadata

Simple upload (PUT `/content`) sends raw binary — there's no way to include `fileSystemInfo` in the same request. Files ≤4 MiB uploaded via simple upload get server receipt timestamps. A post-upload PATCH to `UpdateFileSystemInfo` is required when mtime preservation matters.

That immediate follow-on PATCH can itself briefly race the new item's
item-by-ID visibility and return `404 itemNotFound` even though the upload
response already returned a concrete item ID. Runtime policy: the graph
boundary owns a short exact retry for the post-simple-upload mtime PATCH only;
the rest of the system still treats simple-upload finalization as one outcome.

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
- full E2E websocket tests should seed the watched subtree before daemon
  startup, then time websocket-specific mutations only against that already
  materialized subtree instead of coupling the assertion to fresh parent
  creation plus immediate delta visibility

### Deletion/Creation Ordering

When a file is renamed and a new file is created with the old name, the delta response may list the creation before the deletion. Processing in order causes 409 Conflict errors. The normalization pipeline reorders deletions before creations within each parent ID group.

### Duplicate Items in Single Batch

The same item can appear multiple times in a single delta response page or across pages. The normalization pipeline deduplicates by keeping only the last occurrence of each item ID.

### URL-Encoded Item Names

Some responses return item names with URL encoding (e.g., `%20` for spaces, `%E6%97%A5%E6%9C%AC%E8%AA%9E.txt` for `日本語.txt`). This is inconsistent — most responses return plain UTF-8. The normalization pipeline decodes all item names unconditionally.

### OneNote Package Items

Items with a `package` facet (`type: "oneNote"`) have no `file` or `folder`
facet. Database schemas expecting every item to have a type will fail. Current
product boundary: Graph normalization keeps package items as provider truth for
direct file operations. Sync observation filters package items because they are
not normal file/folder content.

### Delta Endpoint Consistency Lag

The delta endpoint (`/drives/{driveID}/root/delta`) aggregates changes from a different consistency domain than REST item endpoints. After a mutation (e.g., `DELETE /items/{id}`), a direct `GET` on the item returns 404 within seconds, but the delta endpoint may not include the change for 5-60+ seconds. This causes the delta response to lag behind the observable state of individual items.

Microsoft acknowledges this: "Due to replication delays, changes to the object do not show up immediately [...] You should retry [...] after some time to retrieve the latest changes."

Runtime and test policy:

- incremental one-shot sync treats this as normal eventual consistency rather
  than spinning hidden settle loops after every observed mutation
- a direct REST item read succeeding elsewhere is not proof that the current
  incremental delta pass is stale or incorrect
- the explicit one-shot strong-freshness path is `sync --full`, which
  enumerates remote truth instead of trusting one incremental delta pass
- delta-sensitive live tests assert eventual user-visible convergence
  (downloaded file appears locally, local delete propagates, conflict resolves)
  rather than first-pass delta visibility after a direct REST read
- scheduled/manual verifier runs measure this consistency window explicitly in
  the shared E2E `timing-summary.json` artifact, while the sibling
  `quirk-summary.json` artifact records the repo-owned retry/fallback
  classifications that fired during those waits, so CI can track remote write
  visibility, remote delete disappearance, scope-transition readiness,
  sync-convergence p50/p95, and the exact quirk families that were absorbed
  instead of treating the lag as opaque noise

### Folder-Scoped Delta Readiness Lag

On Personal drives, a newly created folder can become path-resolvable before
its folder-scoped delta feed is ready. A direct `GET /drives/{driveID}/root:/path:`
can return `200` for the folder while the immediate first
`GET /drives/{driveID}/items/{folderID}/delta` still returns HTTP 404
`itemNotFound`. This showed up in the fast E2E lane on April 6, 2026 for a
fresh separately configured shared-folder mount bootstrap: the selected folder
existed and contained files, but initial scoped delta still lagged the folder
lookup path.

Runtime policy:
- treat this as a Graph readiness quirk, not as proof the configured path is invalid
- when a personal mount-root bootstrap already resolved the folder by path but
  folder-scoped delta still fails, fall back to recursive subtree enumeration
  for that root instead of failing the whole sync pass
- keep retrying scoped delta on later passes once the folder feed becomes ready

### Ephemeral Deletion Events

Delta deletion events are delivered **exactly once**. If the client's token window advances past a deletion (including via a zero-event response that returns a new token), the deletion is permanently missed. A subsequent incremental delta call will never report that deletion again. Fresh delta (no token) enumerates only existing items — it never reports deletions. This means incremental delta is the **only** way to learn about deletions, and it has exactly one chance to deliver each one.

The combination of consistency lag (above) and ephemeral deletions creates a window where: (1) a deletion is performed, (2) the client calls delta before the deletion propagates to the change log, (3) delta returns zero events + new token, (4) the client saves the new token, permanently skipping the deletion.

Mitigation requires both a zero-event token guard (don't advance token on empty responses) and periodic full remote refresh (enumerate all items, detect orphans in baseline).

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
- the current production retry budget is 6 total attempts with 250ms base, 2x
  multiplier, no jitter, and 4s max
- transfer-manager callers and sync workers do not add their own second retry
  loop for this quirk; the graph boundary is the single owner

### Pre-Authenticated Download URL 401

Graph can return a fresh `@microsoft.graph.downloadUrl` that immediately
responds with HTTP 401 when the client uses it without an Authorization header.
This has been observed for a shared-folder sentinel after the item metadata and
parent child listing both returned HTTP 200, so the item was discoverable while
the pre-authenticated content URL itself was unusable.

Runtime policy:

- authenticated Graph requests keep their ordinary 401 token-refresh behavior;
  this is not treated as evidence for a general post-refresh 401 retry
- `graph.Client.Download()` and `DownloadRange()` own the exact recovery path
- recovery is narrow: only pre-authenticated download content requests, only
  HTTP 401, and only one metadata refetch to obtain a fresh download URL
- HTTP 403 remains a permission failure; the client does not refetch metadata
  or retry content downloads for forbidden shared files

### Transient Failures on Immediate Post-Simple-Upload Mtime Patch

After a small-file simple upload succeeds, the immediate
`PATCH /drives/{driveID}/items/{itemID}` used to restore
`fileSystemInfo.lastModifiedDateTime` can return HTTP 404 with Graph code
`itemNotFound`, or a transient 502/503/504 server failure, even though the
upload response already returned that item ID.

Observed local evidence on April 8, 2026 during `go run ./cmd/devtool verify default`:

- `put /onedrive-go-e2e-1775667044557991000/perm-test.txt` used simple upload
  and received item ID `BD50CF43646E28E6!s0db1ece8e28d4085845e623128c01e29`
- the immediate follow-on `PATCH /drives/bd50cf43646e28e6/items/BD50CF43646E28E6!s0db1ece8e28d4085845e623128c01e29`
  returned HTTP 404 `itemNotFound` with request ID
  `c597aa4d-e4c5-4c89-8888-fa9a07ab36db`
- the cascading fast-suite failure later surfaced in
  `TestE2E_RoundTrip/rm_permanent` only because the preceding `put` had already
  failed on that false negative
- local `go run ./cmd/devtool verify e2e-full --classify-live-quirks` on
  April 23, 2026 hit the same boundary again in
  `TestE2E_SyncWatch_FileDeletion`
- the simple upload itself succeeded and returned item ID
  `BD50CF43646E28E6!s5bf7484a62804c52a3304385a4586cfd`, but the immediate
  follow-on `PATCH /drives/bd50cf43646e28e6/items/BD50CF43646E28E6!s5bf7484a62804c52a3304385a4586cfd`
  returned HTTP 504 `UnknownError` with request ID
  `0bb61a88-1a17-44e0-bed9-e92c3b6bc0b7`
- the later watch deletion failure was secondary: the file content already
  existed remotely, but the failed finalization PATCH left the upload in a
  false-failed state that blocked the later upload-only delete from being
  planned as a remote delete

Runtime policy:

- only the immediate post-simple-upload mtime PATCH gets the retry
- retry remains narrow: only HTTP 404 `itemNotFound` or transient 502/503/504
  server failures, and only on the simple-upload finalization path
- the current production retry budget is 8 total attempts with 250ms base, 2x
  multiplier, no jitter, and 16s max
- direct `UpdateFileSystemInfo()` calls outside simple-upload finalization stay
  strict; callers should not assume all metadata PATCH paths are retried

### Transient 404 on Server-Side Copy Into a Freshly Visible Destination

After a destination folder has become readable by path, the immediate
`POST /drives/{driveID}/items/{itemID}/copy` can still return HTTP 404 with the
message `Failed to verify the existence of destination location`. This means
Graph's copy verifier can lag behind ordinary path reads for the same folder.

Observed local evidence on April 10, 2026 during `go run ./cmd/devtool verify e2e-full`:

- `mkdir /e2e-cp-folder-1775879036248706000/dest` succeeded
- `GET /drives/bd50cf43646e28e6/root:/e2e-cp-folder-1775879036248706000/dest:` returned `200`
- the immediate `POST /drives/bd50cf43646e28e6/items/BD50CF43646E28E6!s75b8ff1dfb2846adbb1003d999d52233/copy`
  still returned HTTP 404 with request ID `f7f79148-fe82-4350-8bc7-a620c0598a1b`
  and message `Failed to verify the existence of destination location`

Runtime policy:

- only the copy start request gets the retry
- retry remains narrow: only HTTP 404, only when the Graph message contains
  `destination location`
- the current production retry budget is 6 total attempts with 250ms base, 2x
  multiplier, no jitter, and 4s max
- post-success listing/readback still belongs to normal path-convergence or
  caller polling; the graph boundary owns only the start-copy false negative

### Folder Create Can Return Success Status With An Empty Body

Graph can return HTTP success for `POST /drives/{driveID}/items/{parentID}/children`
and still provide an empty response body. That leaves the client in an
ambiguous state: replaying the create is not safe because the first request may
already have committed, but blindly trusting success would lose the created
item ID and can surface a false `mkdir` failure even when the folder appears
seconds later.

Observed evidence on April 8, 2026 during `go run ./cmd/devtool verify
e2e-full --classify-live-quirks`:

- `mkdir /e2e-cp-file-1775671921484525000` in `TestE2E_Cp_File` got HTTP 200
  from `POST /drives/bd50cf43646e28e6/items/root/children`
- the response body was empty enough that `CreateFolder()` failed on
  `decoding create folder response: EOF`
- the request ID on that ambiguous success response was
  `4b1bc4e6-0a58-4d5c-bda6-95c14326203f`

Runtime policy:

- `CreateFolder()` treats only an empty or whitespace-only success body as
  ambiguous success
- it never retries the `POST`; instead it re-lists the parent collection under
  the bounded post-mutation visibility budget and returns the matching folder
  once it becomes readable
- if the folder still is not discoverable after that budget, the operation
  fails loudly so callers do not report success without authoritative item
  identity

<a id="post-mutation-visibility-lag"></a>
### Post-Mutation Path Reads Can Lag Successful `mkdir`, `put`, `mv`, And Sync-Created Writes`

Graph can acknowledge a metadata-changing mutation and still return
`404 itemNotFound` on an immediate follow-on path lookup for the destination.
This is a different consistency gap from the item-by-ID download metadata
misfire above: the mutation itself succeeded, but the path-based read model has
not converged yet.

Observed evidence on April 5, 2026 during `go run ./cmd/devtool verify
e2e-full`:

- `put /e2e-sync-ee-1775448127403708000/conflict-file.txt` completed after the
  remote edit step of the historical nightly E2E
  `TestE2E_Sync_EditEditConflict_ResolveKeepRemote`
- the immediate follow-on `stat /e2e-sync-ee-1775448127403708000/conflict-file.txt`
  kept returning HTTP 404 `itemNotFound` with request ID
  `0d76a7d9-c2b4-4eec-acd2-de29e5aec5c7` until the test's 30s poll window
  expired

Runtime policy:

- CLI mutation commands own a bounded post-success visibility wait
- `put` also resolves an already-expected parent path through the same bounded
  visibility gate before uploading a child, so a transient parent-path 404
  after a prior successful command is treated as the same convergence family
- when the exact path route still lies during that visibility wait, the client
  confirms the path by exact-name parent/ancestor listing before spending more
  retry budget on the same stale path lookup
- `mkdir`, single-file `put`, and `mv` retry destination-path reads on exact
  `itemNotFound` using a deterministic schedule that ramps
  `250ms`, `500ms`, `1s`, `2s`, `4s`, `8s`, `16s`, then holds the `32s` cap
  for three capped sleeps, which yields about `95.75s` of scheduled wait
  before request overhead and roughly a two-minute wall-clock budget
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
- the production schedule was therefore first extended to include the final
  `16s` step before surfacing `ErrPathNotVisible`

Observed extension trigger on April 9, 2026 during `go run ./cmd/devtool verify
default`:

- fast E2E `TestE2E_RoundTrip/rm_permanent` proved the same family could
  outlive the old ~32-second total budget after earlier substeps had already
  created, listed, and partially cleaned up `/onedrive-go-e2e-1775753944282690000`
- the follow-on `put /onedrive-go-e2e-1775753944282690000/perm-test.txt` kept
  failing parent resolution for `/onedrive-go-e2e-1775753944282690000` through
  repeated exact-path `404 itemNotFound` plus root-children listings that still
  omitted the folder; representative late request IDs from that failing run
  included `54cd8b32-1efd-4927-8583-faa972d45e7a` and
  `38ebde5b-9ff5-487a-89db-45b85f704e4b`
- the production schedule was therefore widened again to keep the `32s` cap for
  three capped sleeps, which yields about `95.75s` of scheduled wait before
  request overhead and roughly a two-minute wall-clock budget before surfacing
  `ErrPathNotVisible`
- fast E2E helpers mirror that split: generic fixture seeding waits for
  user-visible availability, while exact-route assertions (`stat` or other
  exact-path probes) use dedicated exact-route waits so unrelated tests do not
  overassert the stricter read model

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

Observed extension on April 7, 2026 during the historical isolated repro for
`TestE2E_Sync_DeleteSafetyThreshold`:

- repeated sibling deletes in `/e2e-sync-bigdel-1775633571947871000` reached a
  state where `rm /e2e-sync-bigdel-1775633571947871000/file-10.txt` failed
  before the delete route, because the initial exact path lookup returned
  `GET .../root:/e2e-sync-bigdel-1775633571947871000/file-10.txt: = 404
  itemNotFound`
- the same setup had created `file-10.txt` successfully earlier in the run,
  so one-shot exact-path resolution was not trustworthy enough to declare the
  delete target gone
- runtime now treats that as the same delete-convergence family: delete intent
  stays authoritative on the path, and the session may need to consult the
  parent collection before deciding whether the file still exists

Runtime policy:

- CLI path-oriented deletes use path-authoritative recovery, not one-shot
  item-ID authority
- `rm` first resolves its target through
  `driveops.MountSession.ResolveDeleteTarget()`
  instead of a one-shot exact path lookup
- `rm`, `mv --force`, and `cp --force` delete through
  `driveops.MountSession.DeleteResolvedPath()` /
  `PermanentDeleteResolvedPath()`
- when the initial exact path lookup or later delete returns exact
  `404 itemNotFound`, the session:
  - first retries target resolution through the parent collection when the
    exact path route says `itemNotFound`
  - if that parent-path collection route also lies with `itemNotFound`, it
    resolves the parent folder through ancestor collections and then lists the
    parent's children by item ID instead of trusting the first parent path miss
  - accepts a now-missing path as success only when both the exact-path route
    and parent collection miss
  - retries delete against the current resolved item when the path still exists
  - treats a final parent-list-only hit that still deletes as `itemNotFound`
    as stale positive evidence and returns success only after the bounded
    convergence schedule exhausts
  - uses the same bounded deterministic convergence schedule as
    post-success path visibility
- `rm` additionally waits for the non-root parent path to become readable again
  before printing success, but once delete intent has already proved the
  target path is gone it downgrades a pure bounded parent `PathNotVisibleError`
  to a warning instead of surfacing a false failure
- live tests that assert remote existence after a successful write-producing
  sync pass must use the longer remote-write visibility budget rather than the
  generic 30-second poll helper
- live-test fixture setup is intentionally split:
  - fixture `put` retries may absorb only the documented command-window
    fresh-parent create and parent-resolution lag families that the failing
    CLI `put` itself observed
  - generic fixture readiness waits prove user-visible availability (exact stat
    success or parent listing visibility), not exact-route convergence; when
    exact stat still lags behind a visible parent listing they record the
    `post_mutation_destination_visibility_lag` recurrence from that follow-on
    wait instead of attributing it to the earlier `put`
  - tests that are explicitly validating exact-path reads must still poll the
    exact route directly instead of inheriting the looser fixture-seed contract

<a id="strict-auth-preflight-quirks"></a>
### Transient 403 on `/me/drives` (Discovery/Auth Projection Lag)

`GET /me/drives` can return HTTP 403 with Graph code `accessDenied` while
`GET /me` succeeds with the same saved login. Earlier evidence pointed at token
propagation after refresh, but later CI and local verifier evidence broadened
the observed shape: Graph's drive-discovery/auth projection can lag behind
other authenticated surfaces such as `/me` even when the caller did not just
refresh the token.

Observed CI evidence:

- scheduled `e2e_full` run on April 5, 2026 (`23999446320`) got `GET /me = 200`
- the same test then got 3 consecutive `/me/drives` failures over about 3.1
  seconds under the old production retry budget
- a later `whoami` command in the same run succeeded again roughly 10-12
  seconds later, so the failure window was transient rather than a durable
  auth rejection
- local `verify default` on April 8, 2026 hit the same family in
  `TestE2E_AuthPreflight_Fast/personal_kikkelimies123@outlook.com`: `/me`
  succeeded, then 10 consecutive `/me/drives = 403 accessDenied` responses
  exhausted the strict 30-second preflight window
- request IDs from that failed strict-preflight window:
  `312b53d7-f478-48c4-9f36-363067a2c1b6`,
  `b58ff5b7-6764-4223-9c83-ceb6f1ca3222`,
  `b0cec41d-7b15-4c81-8391-312edf2239d0`,
  `1cf97115-e2ab-4330-9706-c39df8294dcb`,
  `03c81db2-eb2f-4d77-b142-1a1b481f0cd9`,
  `0db09ec7-3a42-4ae7-aba0-fbf45beeb6fc`,
  `9c3ff205-3eaf-4dc8-83fa-8b80afdd9d02`,
  `69c9fbf9-ffa8-49e8-94f0-558cea646da9`,
  `24529051-0dd1-49a9-a908-f0c1b53930e4`,
  `a7fe6793-d731-4c0d-b6f5-a424e9de2d45`
- an immediate isolated rerun of the same preflight later passed for that
  account in about 1.4 seconds, confirming the strict preflight can still
  lose a transient consistency race even when the account recovers quickly

Runtime policy:

- `graph.Client.Drives()` owns the exact quirk retry
- retry remains narrow: only `/me/drives`, only HTTP 403, only Graph code
  `accessDenied`
- the current production retry budget is 5 total attempts with 1s base, 2x
  multiplier, ±25% jitter, and 60s cap
- after that budget is exhausted, CLI discovery surfaces degrade explicitly
  instead of misclassifying the account as auth-required:
  - `status` keeps the authenticated account visible and falls back to
    `/me/drive` when possible
  - `drive list` keeps configured/offline state, uses `/me/drive` fallback for
    the primary drive, and continues independent shared-folder and SharePoint
    discovery
- retry exhaustion now also carries per-attempt request IDs, HTTP statuses,
  and most-specific Graph codes in a typed `graph.QuirkRetryError`, so CLI
  degraded-mode logs and incident triage can preserve the exact observed
  projection-lag evidence without changing runtime behavior
- callers that only need those facts project them through
  `graph.ExtractQuirkEvidence`; the quirk wrapper remains the behavior-bearing
  error so retry classification does not leak into a second generic error model
- scheduled/manual `devtool verify e2e-full --classify-live-quirks` may rerun
  the strict auth preflight once for this exact known quirk and only downgrade
  it when the rerun passes; required per-PR lanes stay strict
- required live lanes use a longer repo-owned auth-preflight endpoint budget
  than ordinary 30-second polls, and graph integration tests use a test-only
  drive-discovery policy scoped to the integration timeout, so CI can observe
  recovery without changing the production `graph.Client.Drives()` retry budget

### Transient 504 `ProfileException` on `/me` During Strict Auth Preflight

Strict auth preflight used to treat `GET /me` as a one-shot proof while only
polling `/me/drives`. Local verifier evidence on April 9, 2026 broadened the
auth-preflight quirk family: `GET /me` returned HTTP 504 `GatewayTimeout` with
message `ProfileException` for
`TestE2E_AuthPreflight_Fast/personal_testitesti18@outlook.com`, request ID
`446dc036-f752-4805-9c33-d637eb70975d`, and an immediate rerun later passed.

Repo policy:

- product runtime is unchanged; this did not become a new graph-client
  semantic retry rule
- the repo-owned strict auth preflight now gives `/me` its own bounded retry
  window for transient 502/503/504 or transport-read failures
- durable auth failures such as 401/403 on `/me` still fail the strict
  preflight immediately
- `/me/drives` keeps its broader bounded poll because the documented discovery
  projection lag can manifest as repeated `403 accessDenied` rather than a
  single transient transport/service failure
- strict-preflight failure output now includes the retry decision for each
  attempt (`retry=true|false` plus a stable reason string such as
  `transient_gateway_status` or `drive_catalog_projection_lag`) so verifier
  artifacts can be read directly without reconstructing the classifier from raw
  bodies, and the shared E2E `quirk-summary.json` artifact preserves those
  typed reasons alongside request IDs/statuses for later incident triage

### Slow/Stalled Metadata Response Headers

Ordinary Graph metadata requests can sometimes connect successfully and then
stall for tens of seconds before sending response headers. This was observed
in the scheduled `e2e_full` CI run on April 3, 2026 during setup for the
delete-safety protection test: a normal metadata/path-resolution request hung
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
- live tests that validate name-based shared-folder `drive add` must treat the
  documented no-match shared-discovery error as the same best-effort visibility
  window after first proving the fixture exists by stable remote drive/item ID

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
- when shared-folder discovery omits a folder, `drive add <raw-share-url>` is
  the preferred bypass because it resolves the share directly instead of
  depending on search or `sharedWithMe`
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
  so read-only shared folders surface `403` and enter the normal
  `perm:remote` flow instead of being misclassified as missing

<a id="fresh-parent-child-create-lag"></a>
### Recently Created Parent Folders Can Lag Child Routes

Live `e2e_full` coverage on April 5, 2026 captured a second create-path
consistency gap after folder creation. The parent folder
`/e2e-watch-websocket-1775447250013596000` was already readable by path lookup
(`request-id: c19f75f0-9a85-43e0-8144-0b4be7387226`), but the immediate follow-on
upload-session creation for `first.txt`
(`POST ...:/createUploadSession`, `request-id: d02b9317-d3d5-44ad-a30c-327df8c859d3`)
still returned HTTP 404 `itemNotFound`.

The same family can also hit mount-root children routes after a freshly created
folder is configured as a standalone shared-folder mount. On April 22, 2026 the
isolated shared-folder watch helper could resolve `/e2e-sync-root-...` by path
and derive its item ID, but the immediate `ls /` against that new mount root
still returned `404 itemNotFound` on the root item's `/children` route until
Graph caught up.

Runtime policy:
- `CreateUploadSession()` retries that exact fresh-parent `404 itemNotFound`
  case with a short bounded policy (`6` total attempts, `250ms` base, `4s` max,
  no jitter)
- the retry stays at the graph quirk boundary instead of teaching callers or
  generic transport retry about parent-creation ordering
- when the same small-file create already saw an initial simple-upload
  `404 itemNotFound`, the graph boundary may replay the original simple upload
  under a second bounded create-convergence policy (`7` total attempts, `250ms`
  base, `8s` max, no jitter) after the session path still exhausts on exact
  `itemNotFound`
- flows that already know the authoritative remote `itemID` avoid this parent
  create route entirely and overwrite by item ID instead of re-creating by
  parent path
- live-test fixture setup that only needs a remote file for some later
  assertion should treat this as a known provider timing family and retry the
  whole fixture `put` operation when Graph either exhausts the documented
  child-create retries or still reports `remote path not yet visible` while
  resolving the freshly created parent for that later command
- isolated shared-folder fixture setup must also wait for the configured
  mount-root `ls /` boundary before returning, because owner-drive path
  visibility alone does not prove the mount-root children route is ready for
  later upload/watch coverage
- mount-root remote-read helpers that verify later exact-path visibility or file
  contents through `stat`, `get`, or equivalent CLI read pollers must
  re-establish the same `ls /` boundary before reading and after known
  `resolve item path ... list children for segment ... HTTP 404` failures,
  because the route can regress again after earlier sync or mutation success
- that fixture policy remains narrow: it does not absorb unrelated failures,
  and it does not replace tests that are explicitly asserting exact-route
  behavior after the create succeeds
- if the request still fails after the quirk budget, the upload returns the
  final error without pretending the parent exists

### Shared-Item Identity Response Shape

Graph shared-item identity is most reliably present under `remoteItem.shared`
and `remoteItem.createdBy`, not top-level `shared`. Four-level fallback chain:
`remoteItem.shared.sharedBy` → `.owner` → `remoteItem.createdBy` →
top-level `shared.owner`. The `email` field in identity responses is
undocumented but works on both personal and business accounts.

### AddToOneDrive Shortcut Visibility Header

OneDrive folder shortcuts are represented as `remoteItem` placeholders, but
Graph can omit Add-to-OneDrive shortcuts from children/delta responses unless
the request sends `Prefer: Include-Feature=AddToOneDrive`. This matches the
field behavior reported by the abraunegg client and observed provider traces.
Runtime policy: children listings send `Include-Feature=AddToOneDrive`; delta
requests combine it with `deltashowremoteitemsaliasid` so shortcut placeholders
remain discoverable without giving the sync engine shortcut ownership.
Even with that header, live root listings can transiently omit a known manual
shortcut while shared discovery and direct target traversal still work. The
reverse has also appeared: shortcut placeholders and direct traversal can be
healthy while shared search/list output is temporarily empty for the target.
Live fixture checks therefore use a shortcut-specific propagation budget. When
the shared target remains reachable but the parent root placeholder is still
omitted after that budget, shortcut E2Es skip because the product behavior
under test cannot start without the provider-supplied placeholder.

Observed fixture repair on April 25, 2026: deleting a personal-account shortcut
placeholder by item ID removed only the placeholder. Microsoft documents
`driveItem.remoteItem` and the nested `remoteItem` fields as read-only, and the
folder-create API documents creating normal `folder` items rather than
shortcut placeholders. Community reports describe undocumented `POST
/drive/root/children` bodies with `remoteItem`, mostly for Business/SharePoint
shortcuts with app permissions, but that is not a supported or reliable
contract for recreating personal-account manual fixtures. Runtime policy:
production can delete the placeholder when the user deletes the local projected
root, but recurring live E2E must not exercise placeholder delete/recreate
against manually-created shortcut fixtures.

Shortcut target contents are a separate authority from the parent placeholder.
The parent drive delta/list response exposes the shortcut item and its
[`remoteItem`](https://learn.microsoft.com/en-us/onedrive/developer/rest-api/resources/remoteitem?view=odsp-graph-online)
target identity; child contents require traversing or syncing the target
drive/root itself. Microsoft Graph
[`delta`](https://learn.microsoft.com/en-us/graph/api/driveitem-delta?view=graph-rest-beta)
token expiry still requires a fresh target enumeration, and a deleted parent
placeholder is not a delete of the shared target. The
[abraunegg client](https://github.com/abraunegg/onedrive/blob/master/docs/business-shared-items.md)
follows the same split in practice: process the account/root delta first, then
enumerate shared/remote items through their own target identities and preserve
data when target access is lost. This repo therefore gates managed child starts
on fresh parent shortcut-root state and treats inaccessible child targets during
final drain as retryable lifecycle, not as permission to delete either side's
tree.

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

Observed nightly recurrence on April 22, 2026: the same recipient account's
`drive list --json` call returned zero actionable shared identities on one
invocation and then exposed the known shared-folder fixtures a few seconds
later with no auth or config change in between. Runtime policy stays the same:
exact selector workflows still bootstrap from raw selector/link identity, not
from discovery, and repo-owned live E2E that validates known shared fixtures
waits through one ordinary read-only propagation window before treating the
omission as a provider limitation for that run.

### Search + GetItem Can Still Miss Shared Owner Identity

Even when `search(q='*')` returns actionable shared identity
(`remoteDriveID` + `remoteItemID`), the follow-on
`GET /drives/{driveId}/items/{itemId}` enrichment pass can still omit enough
owner identity that the caller cannot recover the sharer's email/display name.
This is not a reason to drop the item: the selector is still valid even when
owner identity is incomplete.

Runtime policy:
- search remains the primary shared-discovery surface
- actionable results are still enriched with `GetItem`
- if owner identity is still missing after enrichment, `drive list` keeps the
  item visible with a deterministic display name, and both `drive list` and
  `shared` surface an explicit retryable owner-identity gap ("owner
  unavailable from Microsoft Graph; try again later") instead of dropping the
  target or silently relying on omitted owner fields
- deprecated `sharedWithMe` evidence may still be useful for forensics, but it
  is not a runtime discovery fallback

### Cross-Organization Shared Discovery Can Still Be Omitted (Business)

The stale blanket claim that business cross-organization shares are
categorically invisible is too strong. Current evidence is narrower: Graph can
still omit some external or cross-organization shared folders that are visible
in the web UI, even though the supported runtime discovery surfaces do recover
others.

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

Runtime policy: the graph boundary owns this recovery. It queries session
status immediately, adopts the returned upload URL if Graph rotated it, parses
the authoritative resume offset from `nextExpectedRanges`, and treats empty,
malformed, or non-forward replies as hard errors instead of guessing.

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

`parentReference.path` (when present) contains URL-encoded characters (`%20` for spaces). The client decodes it at the Graph boundary and stores the root-relative result on `graph.Item.ParentPath`. A valid root parent path normalizes to the empty string, so `graph.Item.ParentPathKnown` records whether Graph actually supplied a valid path. Malformed encodings or missing `/root:` markers are ignored rather than propagated as bogus paths.

## Case Sensitivity

### OneDrive Is Case-Insensitive

Two files differing only in case cannot coexist in the same OneDrive folder. The API's path-based queries perform case-insensitive matching but can return items from incorrect paths (observed with git repository files where `v1.0.0` and `v2.0.0` produce false matches). `GetItemByPath` mitigates this by post-validating the returned item against the requested path: exact reconstructed path when `parentReference.path` is present, otherwise leaf-name match only. The leaf-only path is intentionally treated as a best-effort fallback, not as exact identity.

## Normalization Pipeline

All delta responses pass through a 5-stage normalization pipeline (`internal/graph/normalize.go`) before the sync engine processes them:

1. Decode URL-encoded names
2. Preserve package items for provider-truth callers; sync filters them later
3. Clear deleted item hashes
4. Deduplicate (keep last occurrence per item ID)
5. Reorder deletions before creations (within each parent)

## Recycle Bin API — Personal vs Business

The `/drives/{id}/special/recyclebin/children` endpoint only works on Business/SharePoint drives. Personal OneDrive accounts return **HTTP 400** (`"The special folder identifier isn't valid"`) because the `special` folder collection does not include `recyclebin` on Personal accounts.

Similarly, `permanentDelete` (used by `recycle-bin empty`) returns **HTTP 405** on Personal accounts — the code already handles this by falling back to regular `DELETE` (`recycle_bin.go`).

**Impact**: `recycle-bin list` and `recycle-bin restore` are Business-only. E2E tests skip on Personal accounts.
