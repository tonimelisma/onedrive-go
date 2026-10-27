# Graph Client

GOVERNS: internal/graph/auth.go, internal/graph/auth_browser.go, internal/graph/auth_device.go, internal/graph/auth_token.go, internal/graph/client.go, internal/graph/client_auth.go, internal/graph/client_construction.go, internal/graph/client_preauth.go, internal/graph/delta.go, internal/graph/download.go, internal/graph/drives.go, internal/graph/drives_identity.go, internal/graph/drives_shared.go, internal/graph/drives_sites.go, internal/graph/errors.go, internal/graph/item_paths.go, internal/graph/item_create.go, internal/graph/items.go, internal/graph/items_copy.go, internal/graph/items_fetch.go, internal/graph/items_mutation.go, internal/graph/items_permissions.go, internal/graph/normalize.go, internal/graph/quirks.go, internal/graph/redaction.go, internal/graph/response_body.go, internal/graph/shares.go, internal/graph/socketio.go, internal/graph/transfer_watchdog.go, internal/graph/types.go, internal/graph/upload.go, internal/graph/upload_session.go, internal/graph/upload_transfer.go, internal/graph/url_validation.go, internal/graphtransport/doc.go, internal/graphtransport/profiles.go, internal/tokenfile/tempfile.go, internal/tokenfile/tokenfile.go

Implements: R-3.1 [verified], R-6.7 [implemented], R-6.8 [verified], R-1.1 [verified], R-1.4 [verified], R-1.5 [verified], R-1.6 [verified], R-1.6.2 [verified], R-1.7 [verified], R-1.8 [verified], R-1.2.5 [verified], R-1.3.5 [verified], R-3.6.4 [verified], R-6.7.8 [verified], R-6.7.9 [verified], R-6.7.10 [verified], R-6.7.11 [verified], R-6.7.12 [verified], R-6.7.13 [verified], R-6.7.16 [verified], R-6.7.17 [verified], R-6.7.18 [verified], R-6.7.22 [verified], R-6.7.23 [verified], R-6.7.26 [verified], R-6.8.4 [verified], R-6.8.6 [verified], R-6.8.8 [verified], R-6.8.14 [verified], R-6.3.4 [verified], R-6.8.16 [verified], R-6.10.6 [verified], R-6.8.17 [verified]

## Overview

All Microsoft Graph API communication flows through `graph.Client`, but Graph-facing HTTP runtime policy is split across `internal/graphtransport` and `driveops.SessionRuntime`. The split is deliberate:

- `graph.Client` is a pure API mapper plus a narrow Graph-quirk normalizer: authentication, endpoint-local quirk retries, and error construction.
- `graphtransport` is the stateless home for Graph-facing `http.Transport` / `http.Client` profile construction.
- `driveops.SessionRuntime` is the single runtime owner of long-lived client reuse, token-source caching, and optional shared throttle coordination for retrying metadata clients.
- `retry.RetryTransport` stays generic. It owns transport retry mechanics, not Graph semantics or caller profile selection.

Callers receive clean, consistent data via `graph.Item` and never deal with API inconsistencies. Callers also choose the right HTTP profile without teaching `graph` about CLI-vs-sync policy.

## Graph HTTP Runtime Policy (`internal/graphtransport`)

`internal/graphtransport` constructs the concrete HTTP profiles used for Graph work:

- `BootstrapMetadataClient(logger)`: retrying metadata client for login/bootstrap/account discovery before account identity is known
- `InteractiveMetadataClient(logger, gate)`: retrying metadata client for one interactive target, with caller-owned shared 429 coordination
- `InteractiveTransferClient(logger)`: retrying transfer client for upload/download/copy-monitor flows
- `SyncClientSet()`: non-retrying metadata/transfer clients for sync classification and sync uploads/downloads

`driveops.SessionRuntime` composes those stateless builders into the three runtime shapes the app actually uses:

- bootstrap metadata for login/account discovery before target identity is known
- target-scoped interactive metadata + shared interactive transfer clients for ordinary CLI work
- shared non-retrying sync clients for sync workers

Bootstrap remains intentionally unscoped. Before the CLI has both caller
identity and remote target identity, it cannot safely infer a narrower shared
throttle domain, so login/account discovery/share-URL resolution use
the bootstrap metadata client and only switch to target-scoped interactive profiles once
the target is known.

`driveops.SessionRuntime` owns the runtime rule that all Graph-facing clients use `Timeout = 0`. Stall detection lives in the transport:

- metadata and transfer transports clone `http.DefaultTransport`
- `ResponseHeaderTimeout` detects servers that accept the connection but stall before sending headers
- dial and TLS handshake deadlines protect connection setup
- keepalives detect dead connections without bounding total transfer duration

`graphtransport` also treats `http.DefaultTransport` being anything other than
`*http.Transport` as an invariant violation. The stdlib guarantees the default
shape; a global replacement would silently drop the tuned clone settings, so
the builder panics instead of constructing a degraded fallback transport.

This boundary exists to prevent one blunt client-wide timeout from becoming a competing owner of caller deadlines. Total operation budgets still belong to caller contexts. Transport stall detection belongs to `graphtransport`.

## Internal Domain Layout

The package keeps one public façade (`graph.Client`) but splits its internal implementation by domain:

- `auth*.go`: device flow, browser callback flow, and token-source persistence
- `drives*.go`: user/drive/site discovery plus shared/search pagination
- `items*.go`: item fetch, mutation, recycle-bin, copy, and permission APIs
- `upload*.go`: upload-session lifecycle vs transfer/chunk execution
- `client_*.go`, `errors.go`, `redaction.go`, `url_validation.go`: cross-cutting boundary helpers

This keeps domain-specific logic close to its tests without reopening the public API surface.

## Ownership Contract

- Owns:
  - `graph`: Graph request construction, authentication flows, token persistence callbacks, Graph-specific normalization, endpoint-local quirk retries, and `GraphError`/sentinel creation
  - `graphtransport`: Graph-facing HTTP transport profile construction and transport defaults
  - `driveops.SessionRuntime`: long-lived client reuse and caller-scoped throttle-gate wiring
- Does Not Own: Config discovery, sync retry scheduling, scope activation, store persistence, or CLI presentation.
- Source of Truth: Graph HTTP responses plus token file contents loaded and written through `tokenfile`.
- Allowed Side Effects: HTTP requests, loopback callback-server startup during browser auth, and token-file I/O through `tokenfile` and rooted managed-state helpers.
- Mutable Runtime Owner: `Client` instances are immutable after construction except for request-scoped state inside individual method calls. Browser auth owns its callback goroutine, server, and result channel only for the lifetime of `LoginWithBrowser`.
- Error Boundary: `graph` is the first translation boundary from wire/auth/HTTP failures into the domain model described in [error-model.md](error-model.md). Higher layers may add context, but they do not reinterpret raw Graph payloads directly.

## Verified By

| Behavior | Evidence |
| --- | --- |
| Auth flows, token persistence, and browser/device login remain Graph-boundary responsibilities. | `internal/graph/auth_test.go`, `internal/graph/auth_browser_test.go`, `internal/graph/auth_device_test.go` |
| Graph request normalization and error translation stay inside the Graph boundary. | `internal/graph/client_test.go`, `internal/graph/errors_test.go`, `internal/graph/normalize_test.go` |
| Drive, shared-item, and upload-session quirks are handled at the Graph edge rather than in CLI or sync. | `internal/graph/drives_test.go`, `internal/graph/drives_shared_test.go`, `internal/graph/upload_session_test.go`, `internal/graph/upload_test.go` |

## Authentication (`auth.go`)

Two OAuth2 flows:
- **Device code flow** (default): user enters code on microsoft.com
- **PKCE authorization code flow** (`--browser`): localhost callback with code verifier

Token refresh is automatic for normal token sources. Tokens are stored as JSON
files via `tokenfile` package (strict JSON — rejects unknown fields).
`tokenfile.Load` returns `ErrNotFound` sentinel (never nil,nil). Dry-run sync
uses the same token source as live sync; silent refresh and `OnTokenChange`
token-file persistence remain part of normal authenticated Graph access.
The device-code boundary intentionally stays narrow: production calls
`oauth2.Config.DeviceAuth` and `DeviceAccessToken` directly, while unit tests
can inject those two calls independently to cover cancel/save/error behavior
without waiting on real polling intervals. One smoke test still runs through
the real oauth polling integration.

## Quirk Normalization (`normalize.go`)

All API quirks handled at the graph boundary — downstream code never sees them:
- DriveId casing normalization (lowercase)
- DriveId truncation fix (zero-pad Personal IDs to 16 chars)
- Item names are opaque and pass through verbatim on every surface (R-6.7.8).
  A live probe on Personal returned `a%20b.txt`, `test%46.txt` and
  `x%2Fy.txt` literally from create, GET, `/children` and `/delta`; the
  earlier unconditional URL-decode renamed such files locally and invented
  subfolders. No surface has been recorded returning an encoded name; if one
  is, decode only that response shape, with a recorded fixture.
- Paginated non-delta item surfaces (`ListChildren`, drive search) keep
  provider-visible package items. Sync-only package suppression is owned by
  `internal/sync/item_converter.go`, not by the Graph client.
- Delta deletion reordering (deletions before creations within each page)
- Missing field recovery (name, size for deleted items)
- Timestamp validation that preserves unknown timestamps as zero time instead of fabricating a replacement, including `null`, missing, invalid, and out-of-range wire values
- `parentReference.path` is usually unavailable in delta and sync primarily
  tracks remote items by ID. When Graph does provide a valid path, it is
  kept verbatim (it is not URL-encoded, L-32) as root-relative `Item.ParentPath`; `Item.ParentPathKnown`
  distinguishes a known root parent from an omitted/invalid path because both
  normalize to an empty string.
- `GetItemByPath` post-validates Graph's response against the requested path. Internal path helpers distinguish exact root-relative paths (when `parentReference.path` survived normalization) from best-effort leaf-only fallbacks. When the exact parent path is unavailable the client logs the fallback at Debug and validates only the leaf name before accepting the result.
- Personal drive discovery is normalized through `/me/drive`: when `/me/drives` returns one or more `driveType == "personal"` entries, the client replaces all of them with the single authoritative primary drive from `GET /me/drive` before returning the list to callers.
- Exact transient 403 retry on `GET /me/drives` when the Graph code chain contains `accessDenied`, using the named drive-discovery policy (5 total attempts, 1s base, 2x multiplier, ±25% jitter)
- Exact transient 404 retry on `GET /drives/{driveID}/items/root/children` when the Graph code chain contains `itemNotFound`
- Exact transient 404 retry on `GET /drives/{driveID}/items/{itemID}` when `Download()` or `DownloadRange()` are fetching pre-authenticated download metadata and the Graph code chain contains `itemNotFound`
- Exact transient 404 retry on the file create `POST /drives/{driveID}/items/{parentID}/children` when a freshly created parent is already visible by path but the create still reports `itemNotFound`
- Exact transient 404 retry on `POST /drives/{driveID}/items/{itemID}/copy` when Graph says it failed to verify the destination location even though the destination folder is already path-visible

The graph boundary intentionally stops at those exact request/response quirks.
It does not own post-success path convergence such as "mutation succeeded, but
path lookup is still briefly not readable" or "delete-by-ID briefly reports
404 even though the path just resolved." Those behaviors belong to
`driveops.PathConvergence`.

Hashless-file handling is split intentionally across boundaries: `graph` keeps
size, mtime, and eTag truthful when Graph omits hashes, but it does not decide
equality itself. The sync baseline persists those values per side, and the
planner applies the `size + mtime + eTag` fallback only when both remote-side
hashes are absent.

## Delta Queries (`delta.go`)

Paginated delta with normalization pipeline. Returns `[]Item` (clean, normalized). HTTP 410 → `ErrGone` sentinel, caller triggers full re-enumeration. Supports mount-root delta for separately configured shared folders when Graph exposes folder delta for that root.

The client owns its safety guards and Graph-specific request metadata:

- `maxDeltaPages`: upper bound for full delta enumeration
- full delta enumeration fails closed when a provider page has neither
  `@odata.nextLink` nor `@odata.deltaLink`; callers must never receive an
  empty completed cursor after malformed provider pagination
- `maxRecursionDepth`: upper bound for recursive child listing
- `driveDiscoveryPolicy`: transient 403 retry policy for `/me/drives`
- `rootChildrenPolicy`: transient 404 retry policy for `root/children`
- `downloadMetadataPolicy`: transient 404 retry policy for item-by-ID download metadata lookup
- `uploadSessionCreatePolicy`: transient 404 retry policy for create-upload-session requests against freshly created parents
- `copyDestinationPolicy`: transient 404 retry policy for copy requests whose destination folder is already visible by path but not yet accepted by Graph's copy verifier
- `deltaPreferHeader`: prebuilt alias-ID delta header plus `Include-Feature=AddToOneDrive`
- `childrenPreferHeader`: prebuilt `Include-Feature=AddToOneDrive` header for child listing

These are instance fields on `graph.Client`, not package globals. Tests in
package `graph` override them per client instance instead of mutating shared
process state, and the shared `newTestClient` helper installs the fast test
policy across all client-owned retry paths by default so unit tests do not pay
production backoff windows accidentally.

`graph` intentionally stops at the Graph-boundary retry. After the
five-attempt `/me/drives` budget is exhausted, the client returns the final
error wrapped in `QuirkRetryError` without inventing auth-required or
degraded-user semantics. `QuirkRetryError` records the quirk name plus the
per-attempt request IDs, HTTP statuses, and most-specific Graph codes that
exhausted the bounded retry budget, while still unwrapping to the terminal
`GraphError` so `errors.Is` / `errors.As` behavior stays unchanged. Callers
that only need those facts can project them through `ExtractQuirkEvidence`
without depending on the behavior-bearing wrapper directly. CLI callers own
the next step:

- `status` degrades to the authenticated account card plus `/me/drive` fallback when possible
- `drive list` degrades to best-known live discovery plus `/me/drive` fallback
- sync and ordinary file operations are unaffected because they do not depend
  on `/me/drives`

## Socket.IO Endpoint Queries (`socketio.go`)

`SocketIOEndpoint(ctx, driveID)` maps Microsoft Graph's drive-root `subscriptions/socketIo` endpoint into a narrow `SocketIOEndpoint` value:

- `ID`: opaque Graph endpoint identifier
- `NotificationURL`: sensitive outbound Socket.IO callback URL, wrapped in a redacted log type
- `ExpirationTime`: parsed RFC3339 expiry when Graph supplies one

The graph boundary validates the returned notification URL before exposing it to callers. Accepted production endpoints must be HTTPS on Microsoft `*.svc.ms` hosts. The client also redacts `notificationUrl` values from structured Graph errors and plain-text URL scrubbing, so websocket callback URLs never appear in logs or surfaced error text.

The Graph client does not own websocket runtime state. It performs one synchronous endpoint fetch and returns validated data; connection lifecycle, Engine.IO / Socket.IO framing, ping/pong, reconnect, and renewal are observation-layer responsibilities. The returned `notificationUrl` is the raw Graph callback URL, not the final RFC6455 websocket URL — observation is responsible for the `/socket.io/?EIO=4&transport=websocket` transform described in the reference layer.

## Item Operations (`items.go`)

GetItem, ListChildren, CreateFolder, CreateFile, MoveItem, CopyItem, DeleteItem. Every check of whether a returned name is the one asked for (`GetItemByPath` verification, created-item read-backs) uses `internal/namekey`, OneDrive's own name equivalence, not `strings.EqualFold` (R-2.12.3, `TestGetItemByPath_PathValidationUsesNamespaceKey`). All operations use `graph.Item` — the clean type after normalization. `Item.ParentPath` carries the decoded root-relative `parentReference.path` when Graph provides it and `Item.ParentPathKnown` records that the path was valid, so callers never need to parse Graph's absolute `"/drives/{id}/root:..."` representation themselves or guess whether an empty parent path means root or unknown. `MoveItemIfMatch` and `DeleteItemIfMatch` are the conditional mutation variants used by sync execution after live preflight; they add `If-Match` when the caller supplies an eTag and map HTTP 412 to `ErrPreconditionFailed`.

`CreateFolder` also owns one narrow ambiguous-success recovery path. If Graph
returns a success status for `POST .../children` but the body is empty, the
client does not retry the non-idempotent create. Instead it confirms the new
folder by re-listing the parent collection under the same bounded
post-mutation visibility budget, because a replayed create could turn a
committed success into a false `nameAlreadyExists` conflict.

Timestamp normalization is intentionally lossy in only one direction: valid Graph timestamps become UTC `time.Time` values, while empty, `null`, invalid, or out-of-range timestamps remain the zero value to mean "unknown". The graph boundary never substitutes `time.Now()` for malformed wire data, because downstream sync logic can safely persist and reason about unknown timestamps as `NULL`/unset state.

## Shared Item Resolution

`shares.go` resolves raw OneDrive share URLs through
`GET /shares/{shareIdOrEncodedSharingUrl}/driveItem`. This is an input alias
boundary only:

- the raw sharing URL is treated as a sharing-grant locator
- the returned `graph.Item` carries the underlying owner-side item identity
- CLI and transfer layers normalize immediately to `(recipient account, remoteDriveID, remoteItemID)`

Discovery APIs do not reliably expose the original inbound share URL, so the
repository never treats that URL as the canonical durable identifier for a
shared item.

## Permission Evaluation

`items_permissions.go` decodes the full Graph permission shape needed for
shared-folder writeability checks, not just the `roles` array. Live testing
showed that the permissions endpoint can mix:

- caller-relevant link grants such as anonymous `view` / `edit`
- owner membership rows for the sharer
- bare `link.webUrl` fields on non-caller rows

So the graph boundary exposes a caller-aware `EvaluateWriteAccess` classifier
instead of a raw "any write/owner role means writable" heuristic:

- link rows only count as caller-applicable when the link facet carries grant
  semantics (`type` and/or `scope`)
- explicit `grantedTo*` rows are matched against the authenticated account
- unrelated owner/write rows are ignored
- ambiguous remaining evidence reports `Inconclusive` so sync can fail open

## Transfers

- `download.go`: streaming download with content URL. A ranged read is bound to a `DownloadVersion` and accepted only as a 206 whose Content-Range starts at the offset and spans the item (`validateContentRange`, `ErrRangeResumeRejected`); for a hashless item, an HTML or JSON answer of another type whose length is not the item's is `ErrUnexpectedDownloadBody` (`checkDownloadBodyType`); hashed items are never gated by type, since the hash decides
- `item_create.go`: new files. `CreateFile` posts the name in a JSON body with `conflictBehavior=fail`; `Upload` and `CreateUploadSession` then send the content by item ID through a session conditional on the created eTag, and a zero-byte file is the created item alone (R-5.6.14, L-01). No name ever goes into a URL: a path route turned `v1.0` into folder `_api` holding `v2.0` (probe 4)
- `upload.go`: upload sessions for every non-empty file (320 KiB-aligned chunks) and the zero-byte simple PUT overwrite
- `shares.go`: raw share-link resolution into underlying shared item identity
- `upload_transfer.go` / `upload_session.go`: existing-item overwrite helpers
  for shared-file `put` by `(driveID, itemID)`

Simple-upload PUTs take an `io.ReaderAt` plus the size and go out with an
exact `Content-Length` and a `GetBody` that opens a fresh `SectionReader`, so
the interactive clients' `RetryTransport` resends every byte on a retry
(R-6.8.19, `TestSimpleUpload_RetryAfter503ResendsFullBody`, now exercised on the
overwrite route, the only simple PUT left). The PUT used to
go out chunked over a one-shot reader, and a retry after a 503 sent zero bytes
and replaced the user's file online with an empty one.

Transfer bodies run under a stall watchdog (`transfer_watchdog.go`): the
download routes and every upload fragment get a request context that is
canceled with `ErrTransferStalled` when the body moves no bytes for
`defaultTransferStallTimeout` (2 minutes; tests shorten
`transferStallTimeout`). It is armed only around body I/O, while a request
body is being written and while a response body is read; the wait for
headers belongs to the transports' `ResponseHeaderTimeout`, and
`RetryTransport` disarms it before every retry or throttle wait
(`retry.WithBackoffObserver`), so a long `Retry-After` is not a stall
(`TestUploadChunk_RetryAfterLongerThanStallTimeoutIsNotAStall`). A stall is
reported as `ErrTransferStalled` alone, never with `context.Canceled` in the
chain (`doPreAuth` and `RetryTransport` return `context.Cause`), because sync
reads a cancellation as a shutdown and would drop the action; sync
classifies a stall as retryable before its context check
(`TestClassifyResult_StalledTransferIsRetryableNotShutdown`). A resumed
download that stalls keeps its partial and is retried; only a refused
resume (`ErrRangeResumeRejected`, 416) starts over
(`TestTransferManager_StalledResumeKeepsPartial`). The transports bound only dialing, TLS and the wait
for headers, so a connection that delivered headers and went quiet held a
worker forever (D-045, `TestDownload_StalledBodyIsAbandoned`,
`TestUploadChunk_StalledBodyIsAbandoned`). The transfer and metadata
transports also ping idle HTTP/2 connections (`graphtransport`,
`TestTransferTransport_PingsIdleHTTP2Connections`).

Mutation answers go through `decodeMutatedItem`: a 2xx whose body is empty
(204), not a driveItem (a 201 carrying `{"error":…}`), or an item without
`id` is read back by ID instead of failing as a JSON "EOF" decode error that
classified a mutation that had succeeded as retry work (D-047, R-6.7.33,
`TestMutationResponsesWithoutItem`). An upload completion without its item
returns `ErrUploadCompletedWithoutItem`, because a create has no ID to read
back; the transfer layer looks it up. Items carry `parentReference.driveType`
as `ParentDriveType`. `MoveItemIfMatch` still decodes strictly; its echo
check belongs with the move-outcome work.

Every non-empty upload, create or overwrite, goes through an upload session
whose creation carries `fileSystemInfo`, so content and mtime land in one
version; zero-byte files are the created item, or a simple PUT overwrite, plus
an mtime PATCH (D-034, live probe 7). The create by body retries the exact
fresh-parent `404 itemNotFound` with `UploadSessionCreatePolicy()`; a
read-only shared folder answers it with the correct `403`, which is not
retried. A create never replaces: 409 `nameAlreadyExists` is `ErrConflict`
(`TestUpload_ExistingNameIsNeverReplaced`). When the content step fails
before its final fragment the created empty item is deleted again with
`If-Match` on its creation eTag
(`TestUpload_FailedContentStepRemovesItsEmptyItemConditionally`).

Overwrites by item ID take the caller's preflight eTag as `If-Match` on both
routes (R-5.6.2, D-022, `TestUploadToItem_OverwriteIsConditionalOnBothRoutes`);
a 412 is `ErrPreconditionFailed` and is never retried without the condition (`TestCreateUploadSessionForItem_PreconditionFailureIsNeverRetriedUnconditionally`); Business behaviour of If-Match on session creation is unprobed.

For zero-byte simple uploads with non-zero mtime, the follow-on `PATCH /items/{itemID}`
used to restore `fileSystemInfo.lastModifiedDateTime` can also race item-ID
visibility and return transient `404 itemNotFound`, or hit a bounded
502/503/504 service failure, even though the upload response already returned
the new item identity. The graph boundary retries that exact
post-simple-upload patch with a bounded policy tuned for the longer
mount-root visibility lag observed in live coverage for separately configured
shared-folder mounts. Direct
`UpdateFileSystemInfo` calls outside the immediate simple-upload finalization
path remain strict. When the retry is exhausted the content has still landed,
so `Upload`/`UploadToItem` return the uploaded item together with a
`*MtimeNotAppliedError` instead of a bare failure.

## Paged Listings

Every paged listing follows `@odata.nextLink` to completion, and every page
loop is bounded.

Both halves were incomplete. `/me/drives` and `/sites/{id}/drives` decoded a
single page and stopped -- and the response type did not even declare the
continuation field, so dropping it was undetectable. That failure is silent in
the worst way: the caller receives a short list that looks complete, and a
drive simply does not exist as far as the rest of the program is concerned.

The bound matters for the opposite reason. Delta already refused to follow more
than a fixed number of pages because a circular continuation would otherwise
loop until the process died; the plain listings had no such guard. They share
one now.

Not every listing is paged. `SearchSites` takes a caller-supplied limit and
passes it as `$top`, so its truncation is requested rather than accidental.

## Response Body Bounds

Every response body this package buffers whole is capped. Error bodies use
`maxErrBodySize`; the create-folder success body uses `maxItemResponseSize`.

That last one is the only success path that buffers rather than streams, and
it does so for a reason: Graph sometimes returns an empty body on a successful
create, which is indistinguishable from a real item to a streaming decoder, so
the bytes are read first and an empty body triggers a follow-up read of the
created folder. Buffering is what makes the cap necessary -- without it the
server chooses how much memory the client spends. An oversized body is
reported as an error rather than truncated, because a truncated JSON document
fails as a confusing syntax error that hides the real cause.

## Error Handling (`errors.go`)

Sentinel errors: `ErrGone` (410), `ErrNotFound` (404), `ErrThrottled` (429), `ErrConflict` (409). Error response bodies are read with a 64 KiB cap (`io.LimitReader`) to prevent unbounded memory allocation from malformed responses. HTTP 423 (Locked) from SharePoint co-authoring or an open Office file is `ErrLocked`; the transport does not retry it (locks persist for minutes to hours), and sync records it as retryable `resource_locked` retry work whose backoff grows with each attempt.

`GraphError` preserves Graph's structured error metadata: `Code`, `InnerCodes`, capped `RawBody`, and helper methods `MostSpecificCode()` / `HasCode()`. `Message`, `RawBody`, and `Error()` are sanitized before exposure so bearer tokens and pre-authenticated URLs are redacted even when Graph echoes them back in an error payload. Quirk retries key on the code chain first. One-off incident signatures without recoverable payload evidence stay in the reference layer and are not special-cased in runtime classification.

Bounded documented-quirk retry exhaustion is surfaced as `QuirkRetryError`.
That wrapper is part of the same wire-to-domain boundary: it adds quirk-level
attempt evidence for caller logs and verifier output, but it does not create a
second error classification scheme. Callers still branch on the unwrapped
terminal cause (`ErrForbidden`, `ErrNotFound`, `*GraphError`, and so on), then
optionally project the attached retry evidence through `ExtractQuirkEvidence`
when they intentionally degrade or summarize verifier output.

Each documented quirk has an exported name constant (`graph.Quirk*`, for
example `QuirkUploadSessionCreateTransient404`). That name is the stable
identifier: it is `QuirkRetryError.Quirk`, and it is the `graph.QuirkLogKey`
(`graph_quirk`) field on the one debug record logged when a budget runs out
(per-attempt records use `quirk`). Code and tests that must recognize a quirk
use `errors.As` in process or that log field across a process boundary
(the live-test fixture classifier reads it from `--debug` stderr), never the
error text. `QuirkRetryError.Error()` is plain words for the symptom ("OneDrive
kept reporting the upload's destination folder as missing, even after 7
tries: ...") and deliberately does not contain the quirk name, so rewording it
cannot break a classifier.

`dispatchRequest` wraps every response body so a read failure other than
`io.EOF` carries `*ResponseReadError` (cause reachable through `Unwrap`). It
tells "the connection dropped while OneDrive was answering" apart from a local
write failure returned by the same `io.Copy`, which net/http cannot: it reports
body read failures without the `*url.Error` it uses before the response.
`CheckSavedLogin` validates a token file without building a token source or
logging, for health checks that must not repeat the "loaded saved token" record
of the real `TokenSourceFromPath` load.

This package owns the wire-to-domain normalization step for remote failures:
raw HTTP and Graph payloads become `GraphError` values plus sentinels such as
`ErrGone` and `ErrUnauthorized`. Retry, persistence, and user-facing decisions
consume that normalized boundary contract via [error-model.md](error-model.md).

**Meaning predicates** — `IsQuotaExhausted` and `IsNotSupported` answer "is the
drive full" and "did OneDrive refuse this operation" from the most specific
evidence first (code chain and message, then status), because OneDrive
reports a full drive as 507, 403 `quotaLimitReached` or 500 "Insufficient
Space Available", and a refusal as 501, 505 or `notSupported`. The sync
server-answer table and CLI presentation both consume them
(`TestIsQuotaExhaustedAndIsNotSupported`).

**Timestamp range** — OneDrive Personal accepts `fileSystemInfo` timestamps from
1900-01-01T00:00:00Z (`EarliestFileSystemTime`) and answers anything earlier
with a misleading `404 itemNotFound` (live probe L-26). `UpdateFileSystemInfo`
relabels that 404 as `ErrTimestampOutOfRange`, keeping the status and request
ID, so no caller reads a live item as deleted
(`TestUpdateFileSystemInfo_Pre1900NotFoundIsRangeError`).

The client never sends such a time: every `fileSystemInfo` it writes
(upload-session creation and `UpdateFileSystemInfo`) goes through
`newFileSystemInfo`, which omits the zero time (OneDrive would silently
replace it with the current time) and clamps earlier times to
`EarliestFileSystemTime`, warning once per client
(`TestFileSystemInfo_ClampsPre1900AndNeverSendsZero`). A file whose local
modification time is before 1900 is therefore uploaded with 1900-01-01 as
its OneDrive time, while the baseline keeps its real local time. The
relabelling stays as a defense; sync treats it as a generic per-file failure
and has no user-visible condition for it. Items take their `ModifiedAt` from
`fileSystemInfo.lastModifiedDateTime`, falling back to the top-level field,
which renames and moves bump (D-046, `TestToItem_PrefersFileSystemInfoModifiedTime`).

**Malware** — the `malware` facet decodes to `Item.MalwareDetected`.
`Download`/`DownloadRange` refuse a flagged item with `ErrMalwareDetected`
before requesting content, and a content-host 403 carrying `malwareDetected`
or an `X-Virus-Infected` header is relabelled the same way, so sync records
one `malware_detected` issue instead of probing a permission boundary
(`TestDownload_MalwareFacetRefusesWithoutFetchingContent`,
`TestDownload_MalwareAnswersFromContentHost`). The exact Personal answer is
still to be confirmed by live probe L-19 (EICAR); both signals are
documented, and each is safe if it never appears.

**`/content` fallback** — a file without a `downloadUrl`, or whose refetched
`downloadUrl` is refused again, downloads through `GET /items/{id}/content`
(`downloadViaContentEndpoint`). The first leg goes through the normal
authenticated `doOnce` path, so the 401 forced refresh and `ErrAuthTransient`
apply, but on a copy of the HTTP client whose `CheckRedirect` returns
`http.ErrUseLastResponse` (`requestRoute{keepRedirect: true}`): the bearer is
never carried across the 302. The `Location` is validated against the
content-host allow-list and fetched through the unauthenticated pre-auth path.
Live probe L-20 (Personal) showed `my.microsoftpersonalcontent.com` serves
the file without the bearer. A resumed download (offset > 0) goes through the
same `validateContentRange` check as the tempauth route: 206 with a
`Content-Range` starting at the offset and spanning the item, on whichever
leg serves the bytes; anything else is `ErrRangeResumeRejected`, never a full
body appended at the offset. The version binding is checked before either
route is used (`TestDownload_ContentEndpointNeverSendsBearerToRedirectHost`,
`TestDownload_ContentEndpoint401ForcesRefresh`,
`TestDownloadRange_ContentEndpointResumes`,
`TestDownloadRange_ContentEndpointRefusesAFullBodyAtAnOffset`,
`TestDownload_ContentRedirectToUntrustedHostRefused`).

**User-Agent** — `graph.UserAgent(version)` is the one User-Agent:
`ISV|onedrive-go|onedrive-go/<version>`, Microsoft's decorated form for
attributing traffic (MS-190). Every CLI and sync construction site uses it
(`TestUserAgent_DecoratedFormOnEveryRequest`).

**Gateway diagnostics** — `GraphError.AgsDiagnostic` keeps the
`X-Ms-Ags-Diagnostic` header, and `QuirkRetryAttempt` carries it, for support
escalation next to the request ID (`TestGraphError_KeepsAgsDiagnosticHeader`,
`TestBuildQuirkRetryAttempt_CarriesAgsDiagnostic`).

**RetryAfter Header** — `RetryAfter time.Duration` field on `GraphError`. Parsed from `Retry-After` header for 429 and 503 responses. Implements: R-6.8.6 [verified]

## Transport-Layer Retry

Implements: R-6.8.8 [verified]

Generic retry has been extracted from the graph client into `retry.RetryTransport`, an `http.RoundTripper` wrapper. The graph client no longer has generic retry loops (`doRetry`/`doPreAuthRetry` deleted), throttle state, or transport-layer Retry-After coordination. The transport only replays methods that are safe at that generic boundary (`GET`, `HEAD`, `OPTIONS`, `TRACE`, `PUT`, `DELETE`); unsafe `POST` and `PATCH` calls return their first transient response to the graph operation that owns the mutation semantics. The only client-local retries are documented Graph API quirk normalizations: transient 403 on `/me/drives`, transient 404 on root-child listing, transient 404 on item-by-ID download metadata fetch before a pre-authenticated download begins, transient 404 or 502/503/504 on the immediate post-simple-upload `UpdateFileSystemInfo` PATCH, transient 404 on create-upload-session requests against freshly created parents, and the documented simple-upload fallback path. All other retry, backoff, Retry-After parsing, and shared 429 coordination live below `graph` in the retry/HTTP-profile layer.

`NewClient` accepts an `*http.Client` and returns `(*Client, error)` after validating the base URL and token source. `MustNewClient` is reserved for static/test setup where panic-on-bad-construction is intentional. `driveops.SessionRuntime` is the normal production runtime owner for those clients, and it composes the stateless `internal/graphtransport` builders into the concrete profiles the app needs:

- interactive bootstrap/CLI metadata clients compose `RetryTransport{Policy: retry.TransportPolicy()}`
- interactive target-scoped metadata clients inject a shared `retry.ThrottleGate` so later requests against the same drive or shared target respect server `Retry-After`
- sync clients pass raw `*http.Client`s so failures return immediately for engine-level classification

This keeps caller profile selection out of `graph` while preserving a single generic retry implementation.

Logging levels: per-request and per-item chatter (getting, listing, creating,
moving, deleting, uploading and downloading items, upload-session lifecycle)
logs at Debug; Info is kept for once-per-command milestones (sign-in, delta
enumeration, search). Documented Graph quirks the client handles (the
simple-upload 404 fallback, an empty create-folder success body, 416 chunk
recovery, quirk retry exhaustion, unparseable timestamps or session
expirations) log at Debug, because their outcome is either success or a
returned error. Errors are returned and never also logged above Debug, so the
command reports each failure once and a canceled context during shutdown
prints nothing (`TestTransfersCanceledMidFlightLogNothingAboveDebug`,
`TestTokenBridge_RefreshFailureIsReturnedNotWarned`). Interactive sign-in maps the user-side outcomes (consent
canceled or declined, device code expired) to the plain sentinels
`ErrLoginDeclined` and `ErrLoginCodeExpired` instead of raw OAuth/AADSTS text
(`TestDoLogin_UserSideOAuthOutcomesArePlain`,
`TestBrowserAuthorizationError_AccessDeniedIsPlain`). Every listing follows
Graph's absolute `@odata.nextLink` through `stripBaseURL`, drive listings
included (`TestDrives_FollowsNextLinkAcrossPages`). A non-2xx copy-monitor
reply is a `GraphError`, not a status document
(`TestPollCopyStatus_NonSuccessStatusReturnsGraphError`).

Pre-authenticated requests (upload chunks, downloads) go through `httpClient.Do()` directly. The `doPreAuth` helper sets `req.GetBody` using the `makeReq` factory so `RetryTransport` can rewind request bodies between attempts (e.g., `io.SectionReader` for upload chunks). It also attaches a request-scoped redacted log target such as `preauth:upload chunk`, so transport retry logs never emit the pre-authenticated URL itself. Transport failures come back from `http.Client.Do` as `*url.Error`, whose text embeds the full request URL; `dispatchRequest` scrubs it to scheme and host (`redactURLError`) before the error is wrapped, so surfaced error text and logs never carry the credential either. Implements: R-6.8.17 [verified]

## 401 Token Refresh

Implements: R-6.8.14 [verified], R-6.6.34 [verified]

Transparent token refresh on 401 inside `doOnce()`, independent of retry transport. On 401: refresh token → retry once. If still 401 → return `ErrUnauthorized` (fatal). Auth refresh is lifecycle management, not transient retry — it works regardless of whether the HTTP client has a `RetryTransport`.

Ordinary Graph `401` is intentionally **not** a sync trial or backoff signal.
The graph boundary gets exactly one lifecycle retry: refresh the saved login
and retry the request once. If that still fails, callers receive
`ErrUnauthorized`. Sync treats that as a fatal catalog-owned account-auth
condition for the current pass or watch session, and CLI surfaces present it as
`Authentication required`.

The refresh is forced when needed. `oauth2.ReuseTokenSource` refreshes only an
expired token, so a token the service revoked early came back unchanged and
the second attempt resent it. `tokenBridge.ForceRefresh` (the optional
`forceRefresher` capability) redeems the refresh token now and swaps in the
new source; concurrent 401s serialize on it and share one token-endpoint call
per 30 s (`TestDoOnce_401_ForcesRefreshWhenCachedTokenIsUnchanged`,
`TestTokenBridge_ForceRefreshRedeemsRefreshTokenOncePerBurst`).

A 401 is fatal (`ErrUnauthorized`) only when the refresh itself fails or a
known-fresh token is rejected without a server-side fault. When a freshly
refreshed token still gets "Unable to initialize RPS", `generalException`, or
an `invalid_token` challenge that does not say "expired", the account is
fine and the service failed to validate it, so the error is
`ErrAuthTransient` (RCL-114, MS-198): sync treats it as service-outage
evidence and the watch daemon keeps running
(`TestDoOnce_401_RPSAfterForcedRefreshIsTransient`,
`TestDoOnce_401_TransientSignatures`). A token source that cannot force a
refresh cannot prove freshness, so its 401s stay `ErrUnauthorized`
(`TestDoOnce_401_SameToken_Returns401`).

A 401 from a pre-authenticated URL is `ErrPreAuthURLRejected`, never
`ErrUnauthorized`: those URLs carry their own credential, and its expiry says
nothing about the account. Downloads refetch the URL; sync retries the one
file (`TestDoPreAuth_401IsURLRejectedNotSignIn`). The repository also documents
a spurious `401` for upload-session fragments sent with a Bearer token, handled
at the pre-auth boundary by omitting Bearer auth.

Azure AD outcomes the user can act on map to plain sentinels through one table
(`aadOutcomeError`) shared by the device-code, browser and refresh paths:
`interaction_required`/AADSTS50076/AADSTS50079 to
`ErrLoginInteractionRequired`, AADSTS65001/AADSTS65005 to
`ErrLoginAppNotAllowed`, "not enabled for consumers" to
`ErrLoginConsumerNotEnabled` (`TestAADOutcomeError_Table`,
`TestTokenBridge_InteractionRequiredIsPlainSignInRequired`). At refresh they
also carry `ErrUnauthorized`, since each needs a new interactive sign-in.

`Client` also exposes an optional per-instance authenticated-success hook. The
hook fires only after a normal authenticated Graph request succeeds. CLI code
uses this as a proof boundary to clear stale catalog auth requirements after
live account or drive commands succeed. Pre-authenticated upload/download URLs
never invoke the hook, so pre-auth transport success is not mistaken for proof
that ordinary Graph auth is healthy.

## Runtime Ownership

The graph package intentionally keeps runtime ownership narrow:

- `Client` holds immutable dependencies and per-instance policy knobs only; it owns no background goroutines, channels, or timers.
- Browser auth owns one callback server goroutine plus one result channel per login attempt. `doAuthCodeLogin` starts them, waits for the callback, and shuts them down before returning.
- Token refresh state is delegated to the token source implementation. `graph` consumes it synchronously; it does not run a background refresh coordinator.
- The sole raw `http.Client.Do` production boundary is `client_preauth.go`, where validated pre-authenticated URLs are dispatched.
- `driveops.SessionRuntime` owns the long-lived HTTP clients and any caller-scoped shared throttle gates. It has no background goroutines and no package-level mutable state; the runtime instance is the owner.

## Design Constraints

- `graph/` exposes concrete types, not interfaces (`graph.Client`, `graph.Item`)
- `graph/` does NOT import `config/` — callers pass token paths directly
- `tokenfile/` is a leaf package (stdlib + oauth2 only)
- Token refresh uses a forked `golang.org/x/oauth2` (`github.com/tonimelisma/oauth2`, branch `on-token-change`) with `Config.OnTokenChange` callback for persistence. Tracks upstream proposal `golang/go#77502`.
  - Two consequences are accepted deliberately and must be revisited when the upstream proposal resolves. A `replace` directive does not apply transitively, so this module cannot be consumed as a library by another module without that consumer adding the same `replace`; the product ships as a binary, so this costs nothing today. And the build depends on a personal repository remaining available, which is a single point of failure no amount of pinning removes.
  - The fork is credential-handling code that will not receive upstream security advisories automatically, so `govulncheck` running on every CI verify is the compensating control, not an optional extra. Re-evaluate the fork whenever the upstream proposal moves.
- Pre-authenticated URLs (`@microsoft.graph.downloadUrl`, upload session URLs) bypass the Graph API — use `httpClient.Do(req)` directly, no base URL prefix, no auth headers. Never log these URLs.
- The authenticated-success hook is only a proof signal. It must remain optional, per-client, and side-effect free from the graph package's perspective. Graph owns when the hook fires; callers own any resulting state repair.
- `graphtransport` is the single home for Graph-facing HTTP transport profile construction. Metadata and transfer clients use transport-level deadlines with `Timeout = 0`; no Graph-facing client uses `http.Client.Timeout`.
- Interactive metadata clients may share 429 `Retry-After` coordination through an injected `retry.ThrottleGate`. The coordination scope is the narrowest CLI-known remote boundary: configured drive-root mounts use `(account email, driveID)`, configured mount roots use `(account email, driveID, remoteRootItemID)`, and direct shared-item commands use `(account email, remoteDriveID, remoteItemID)`. Broader account-wide or tenant-wide coordination is intentionally not inferred from ordinary file traffic.
- `driveops.SessionRuntime` selects bootstrap, target-scoped interactive, or sync profiles directly. The CLI and control plane no longer build one cache owner merely to inject it into another.
- Upload URLs are sensitive credentials. The `UploadURL` type implements `slog.LogValuer` with redaction, matching the `DownloadURL` pattern.
- Search API calls URL-escape query parameters to prevent special characters from breaking URL construction.
- `internal/tokenfile` is intentionally pure OAuth token I/O. It persists only the wrapped `oauth2.Token`, rejects unknown JSON fields, and never writes repo-specific metadata alongside the token.
- Token file reads and writes use `internal/fsroot` after config-driven token resolution, so open/read/temp-file creation/chmod/fsync/rename all stay inside one managed-state root capability.
- `client_preauth.go` is the sole raw `http.Client.Do` production boundary. Graph base URLs and pre-auth URLs are validated before a request reaches that call; the remaining inline `gosec` suppression there is intentional because the linter cannot prove the validation flow.
- Graph/API quirk handling requires either captured payload evidence in CI/tests/logs or a reproducible documented observation with enough detail to classify safely. One-off incidents without recoverable payload evidence stay documented as reference notes only and do not become permanent runtime normalization rules.
- Broader throttle coordination beyond the proved remote target is deferred until ordinary file traffic provides reliable bucket identity or equally strong evidence for widening. Today the client intentionally blocks only the narrowest known drive/shared boundary. [planned]
- Upload and async-copy pre-auth URL validation: verify HTTPS scheme and trusted Microsoft hosts on upload session and copy monitor URLs before use. Both copy monitor and upload-session validation explicitly allow Personal-account URLs on `microsoftpersonalcontent.com` after live `e2e_full` coverage captured upload-session URLs on that host family. [verified]
- Audit all `slog.*` calls for potential secret leakage (tokens, pre-auth URLs). [verified]
- Audit all error message strings for embedded secrets — `GraphError.Message` and `RawBody` are redacted before exposure. [verified]
- Test that captures log output and verifies no tokens or pre-auth URLs appear. [verified]
- Authenticated request helpers are package-internal (`do`, `doGetWithHeaders`, and `doRequest`). External callers use higher-level graph operations instead of raw request dispatch. [verified]
- Shared-item discovery uses only `GET /me/drive/root/search(q='*')`. Search
  results may omit owner identity or omit some shares entirely, so callers
  enrich actionable hits through `GET /drives/{driveId}/items/{itemId}` and
  surface caller-level degraded status when live search itself fails. [verified]
- Monitor `search(q='*')` reliability on business accounts for shared item discovery. Current coverage is characterization and unit/spec validation only; no business live fixture exists yet. [planned]
- `PermanentDeleteItem` 405→`DeleteItem` fallback for Personal accounts is a workaround. Remove when MS adds Personal support.

## Struct Tag Policy

Repo-wide JSON/TOML linting treats CLI and persisted payloads as snake_case by default. `internal/graph` is the deliberate exception: Graph wire structs keep upstream camelCase tags, and annotation-backed fields such as `@odata.nextLink` and `@microsoft.graph.downloadUrl` are exempted in lint configuration rather than suppressed inline.
