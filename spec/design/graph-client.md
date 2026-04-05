# Graph Client

GOVERNS: internal/graph/auth.go, internal/graph/auth_browser.go, internal/graph/auth_device.go, internal/graph/auth_token.go, internal/graph/client.go, internal/graph/client_auth.go, internal/graph/client_construction.go, internal/graph/client_preauth.go, internal/graph/delta.go, internal/graph/download.go, internal/graph/drives.go, internal/graph/drives_identity.go, internal/graph/drives_shared.go, internal/graph/drives_sites.go, internal/graph/errors.go, internal/graph/items.go, internal/graph/items_copy.go, internal/graph/items_fetch.go, internal/graph/items_mutation.go, internal/graph/items_permissions.go, internal/graph/normalize.go, internal/graph/quirks.go, internal/graph/redaction.go, internal/graph/socketio.go, internal/graph/types.go, internal/graph/upload.go, internal/graph/upload_session.go, internal/graph/upload_transfer.go, internal/graph/url_validation.go, internal/graphhttp/doc.go, internal/graphhttp/provider.go, internal/tokenfile/tokenfile.go

Implements: R-3.1 [verified], R-6.7 [implemented], R-6.8 [verified], R-1.1 [verified], R-1.4 [verified], R-1.5 [verified], R-1.6 [verified], R-1.6.2 [verified], R-1.7 [verified], R-1.8 [verified], R-1.2.5 [verified], R-1.3.5 [verified], R-3.6.4 [verified], R-6.7.4 [verified], R-6.7.8 [verified], R-6.7.9 [verified], R-6.7.10 [verified], R-6.7.11 [verified], R-6.7.12 [verified], R-6.7.13 [verified], R-6.7.16 [verified], R-6.7.17 [verified], R-6.7.18 [verified], R-6.7.22 [verified], R-6.7.23 [verified], R-6.7.26 [verified], R-6.8.4 [verified], R-6.8.6 [verified], R-6.8.8 [verified], R-6.8.14 [verified], R-6.3.4 [verified], R-6.8.16 [verified], R-6.10.6 [verified]

## Overview

All Microsoft Graph API communication flows through `graph.Client`, but Graph-facing HTTP runtime policy is composed one layer above it in `internal/graphhttp`. The split is deliberate:

- `graph.Client` is a pure API mapper plus a narrow Graph-quirk normalizer: authentication, quirk-specific retries, and error construction.
- `graphhttp.Provider` is the single owner of Graph-facing `http.Transport` / `http.Client` profiles, long-lived client reuse, and optional shared throttle coordination for retrying metadata clients.
- `retry.RetryTransport` stays generic. It owns transport retry mechanics, not Graph semantics or caller profile selection.

Callers receive clean, consistent data via `graph.Item` and never deal with API inconsistencies. Callers also choose the right HTTP profile without teaching `graph` about CLI-vs-sync policy.

## Graph HTTP Runtime Policy (`internal/graphhttp`)

`graphhttp.Provider` constructs the concrete HTTP profiles used for Graph work:

- `BootstrapMeta()`: retrying metadata client for login/bootstrap/account discovery before account identity is known
- `InteractiveForDrive(account, driveID).Meta`: retrying metadata client for ordinary CLI requests against one configured drive, with shared 429 coordination scoped to that exact `(account, driveID)` target
- `InteractiveForDrive(account, driveID).Transfer`: retrying transfer client for upload/download/copy-monitor flows against that drive
- `InteractiveForSharedTarget(account, remoteDriveID, remoteItemID).Meta`: retrying metadata client for one shared-folder or direct shared-item target, with shared 429 coordination scoped to that exact `(account, remoteDriveID, remoteItemID)` boundary
- `InteractiveForSharedTarget(account, remoteDriveID, remoteItemID).Transfer`: retrying transfer client for that shared target
- `Sync().Meta`: non-retrying metadata client for sync classification
- `Sync().Transfer`: non-retrying transfer client for sync uploads/downloads

Bootstrap remains intentionally unscoped. Before the CLI has both caller
identity and remote target identity, it cannot safely infer a narrower shared
throttle domain, so login/account discovery/share-URL resolution use
`BootstrapMeta()` and only switch to target-scoped interactive profiles once
the target is known.

The provider owns the runtime rule that all Graph-facing clients use `Timeout = 0`. Stall detection lives in the transport:

- metadata and transfer transports clone `http.DefaultTransport`
- `ResponseHeaderTimeout` detects servers that accept the connection but stall before sending headers
- dial and TLS handshake deadlines protect connection setup
- keepalives detect dead connections without bounding total transfer duration

This boundary exists to prevent one blunt client-wide timeout from becoming a competing owner of caller deadlines. Total operation budgets still belong to caller contexts. Transport stall detection belongs to `graphhttp`.

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
  - `graph`: Graph request construction, authentication flows, token persistence callbacks, Graph-specific normalization, and `GraphError`/sentinel creation
  - `graphhttp`: Graph-facing HTTP profile construction, transport defaults, long-lived client reuse, and caller-scoped throttle-gate wiring
- Does Not Own: Config discovery, sync retry scheduling, scope activation, store persistence, or CLI presentation.
- Source of Truth: Graph HTTP responses plus token file contents loaded and written through `tokenfile`.
- Allowed Side Effects: HTTP requests, loopback callback-server startup during browser auth, and token-file I/O through `tokenfile` and rooted managed-state helpers.
- Mutable Runtime Owner: `Client` instances are immutable after construction except for request-scoped state inside individual method calls. Browser auth owns its callback goroutine, server, and result channel only for the lifetime of `LoginWithBrowser`.
- Error Boundary: `graph` is the first translation boundary from wire/auth/HTTP failures into the domain model described in [error-model.md](error-model.md). Higher layers may add context, but they do not reinterpret raw Graph payloads directly.

## Authentication (`auth.go`)

Two OAuth2 flows:
- **Device code flow** (default): user enters code on microsoft.com
- **PKCE authorization code flow** (`--browser`): localhost callback with code verifier

Token refresh is automatic. Tokens stored as JSON files via `tokenfile` package (strict JSON — rejects unknown fields). `tokenfile.Load` returns `ErrNotFound` sentinel (never nil,nil).

## Quirk Normalization (`normalize.go`)

All API quirks handled at the graph boundary — downstream code never sees them:
- DriveId casing normalization (lowercase)
- DriveId truncation fix (zero-pad Personal IDs to 16 chars)
- Single-item item names are URL-decoded across fetch, mutation, share-resolution,
  restore, and upload-completion responses
- Paginated non-delta item surfaces (`ListChildren`, `sharedWithMe`, drive
  search) URL-decode names and filter package-only OneNote items before
  callers see them
- Delta deletion reordering (deletions before creations within each page)
- Missing field recovery (name, size for deleted items)
- Timestamp validation that preserves unknown timestamps as zero time instead of fabricating a replacement, including `null`, missing, invalid, and out-of-range wire values
- `parentReference.path` is never returned in delta — items tracked by ID
- `parentReference.path` is URL-decoded and normalized to a root-relative `Item.ParentPath` in non-delta responses
- `GetItemByPath` post-validates Graph's response against the requested path. Internal path helpers distinguish exact root-relative paths (when `parentReference.path` survived normalization) from best-effort leaf-only fallbacks. When the exact parent path is unavailable the client logs the fallback at Debug and validates only the leaf name before accepting the result.
- Personal drive discovery is normalized through `/me/drive`: when `/me/drives` returns one or more `driveType == "personal"` entries, the client replaces all of them with the single authoritative primary drive from `GET /me/drive` before returning the list to callers.
- Exact transient 403 retry on `GET /me/drives` when the Graph code chain contains `accessDenied`
- Exact transient 404 retry on `GET /drives/{driveID}/items/root/children` when the Graph code chain contains `itemNotFound`

Hashless-file handling is split intentionally across boundaries: `graph` keeps
size, mtime, and eTag truthful when Graph omits hashes, but it does not decide
equality itself. The sync baseline persists those values per side, and the
planner applies the `size + mtime + eTag` fallback only when both remote-side
hashes are absent.

## Delta Queries (`delta.go`)

Paginated delta with normalization pipeline. Returns `[]Item` (clean, normalized). HTTP 410 → `ErrGone` sentinel, caller triggers full re-enumeration. Supports folder-scoped delta for shared folders.

The client owns its safety guards and Graph-specific request metadata:

- `maxDeltaPages`: upper bound for full delta enumeration
- `maxRecursionDepth`: upper bound for recursive child listing
- `driveDiscoveryPolicy`: transient 403 retry policy for `/me/drives`
- `rootChildrenPolicy`: transient 404 retry policy for `root/children`
- `deltaPreferHeader`: prebuilt alias-ID delta header

These are instance fields on `graph.Client`, not package globals. Tests in package `graph` override them per client instance instead of mutating shared process state.

## Socket.IO Endpoint Queries (`socketio.go`)

`SocketIOEndpoint(ctx, driveID)` maps Microsoft Graph's drive-root `subscriptions/socketIo` endpoint into a narrow `SocketIOEndpoint` value:

- `ID`: opaque Graph endpoint identifier
- `NotificationURL`: sensitive outbound Socket.IO callback URL, wrapped in a redacted log type
- `ExpirationTime`: parsed RFC3339 expiry when Graph supplies one

The graph boundary validates the returned notification URL before exposing it to callers. Accepted production endpoints must be HTTPS on Microsoft `*.svc.ms` hosts. The client also redacts `notificationUrl` values from structured Graph errors and plain-text URL scrubbing, so websocket callback URLs never appear in logs or surfaced error text.

The Graph client does not own websocket runtime state. It performs one synchronous endpoint fetch and returns validated data; connection lifecycle, Engine.IO / Socket.IO framing, ping/pong, reconnect, and renewal are observation-layer responsibilities. The returned `notificationUrl` is the raw Graph callback URL, not the final RFC6455 websocket URL — observation is responsible for the `/socket.io/?EIO=4&transport=websocket` transform described in the reference layer.

## Item Operations (`items.go`)

GetItem, ListChildren, CreateFolder, MoveItem, CopyItem, DeleteItem. All operations use `graph.Item` — the clean type after normalization. For non-delta item fetches, `Item.ParentPath` carries the decoded root-relative `parentReference.path` when Graph provides it so callers never need to parse Graph's absolute `"/drives/{id}/root:..."` representation themselves.

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

- `download.go`: streaming download with content URL
- `upload.go`: simple PUT (≤4 MiB) and resumable upload sessions (>4 MiB, 320 KiB-aligned chunks)
- `shares.go`: raw share-link resolution into underlying shared item identity
- `upload_transfer.go` / `upload_session.go`: existing-item overwrite helpers
  for shared-file `put` by `(driveID, itemID)`

For create-by-parent uploads, the graph boundary treats a non-zero-size simple
upload `404 itemNotFound` as potentially ambiguous and retries that narrower
case through `createUploadSession` once. Live E2E coverage showed read-only
shared folders returning a bogus 404 on `PUT ...:/content` while
`POST ...:/createUploadSession` returned the correct 403. The fallback keeps
simple upload as the fast path while preserving permission accuracy.

## Error Handling (`errors.go`)

Sentinel errors: `ErrGone` (410), `ErrNotFound` (404), `ErrThrottled` (429), `ErrConflict` (409). Error response bodies are read with a 64 KiB cap (`io.LimitReader`) to prevent unbounded memory allocation from malformed responses. HTTP 423 (Locked) from SharePoint co-authoring is classified as skip, not retryable — locks persist for hours; watch mode retries on the next safety scan.

`GraphError` preserves Graph's structured error metadata: `Code`, `InnerCodes`, capped `RawBody`, and helper methods `MostSpecificCode()` / `HasCode()`. `Message`, `RawBody`, and `Error()` are sanitized before exposure so bearer tokens and pre-authenticated URLs are redacted even when Graph echoes them back in an error payload. Quirk retries key on the code chain first. One-off incident signatures without recoverable payload evidence stay in the reference layer and are not special-cased in runtime classification.

This package owns the wire-to-domain normalization step for remote failures:
raw HTTP and Graph payloads become `GraphError` values plus sentinels such as
`ErrGone` and `ErrUnauthorized`. Retry, persistence, and user-facing decisions
consume that normalized boundary contract via [error-model.md](error-model.md).

**RetryAfter Header** — `RetryAfter time.Duration` field on `GraphError`. Parsed from `Retry-After` header for 429 and 503 responses. Implements: R-6.8.6 [implemented]

## Transport-Layer Retry

Implements: R-6.8.8 [verified]

Generic retry has been extracted from the graph client into `retry.RetryTransport`, an `http.RoundTripper` wrapper. The graph client no longer has generic retry loops (`doRetry`/`doPreAuthRetry` deleted), throttle state, or transport-layer Retry-After coordination. The only client-local retries are documented Graph API quirk normalizations: transient 403 on `/me/drives` and transient 404 on root-child listing. All other retry, backoff, Retry-After parsing, and shared 429 coordination live below `graph` in the retry/HTTP-profile layer.

`NewClient` accepts an `*http.Client` and returns `(*Client, error)` after validating the base URL and token source. `MustNewClient` is reserved for static/test setup where panic-on-bad-construction is intentional. `graphhttp.Provider` is the normal production constructor for those clients:

- interactive bootstrap/CLI metadata clients compose `RetryTransport{Policy: retry.TransportPolicy()}`
- interactive target-scoped metadata clients inject a shared `retry.ThrottleGate` so later requests against the same drive or shared target respect server `Retry-After`
- sync clients pass raw `*http.Client`s so failures return immediately for engine-level classification

This keeps caller profile selection out of `graph` while preserving a single generic retry implementation.

Pre-authenticated requests (upload chunks, downloads) go through `httpClient.Do()` directly. The `doPreAuth` helper sets `req.GetBody` using the `makeReq` factory so `RetryTransport` can rewind request bodies between attempts (e.g., `io.SectionReader` for upload chunks). It also attaches a request-scoped redacted log target such as `preauth:upload chunk`, so transport retry logs never emit the pre-authenticated URL itself.

## 401 Token Refresh

Implements: R-6.8.14 [implemented]

Transparent token refresh on 401 inside `doOnce()`, independent of retry transport. On 401: refresh token → retry once. If still 401 → return `ErrUnauthorized` (fatal). Auth refresh is lifecycle management, not transient retry — it works regardless of whether the HTTP client has a `RetryTransport`.

Ordinary Graph `401` is intentionally **not** a sync trial or backoff signal.
The graph boundary gets exactly one lifecycle retry: refresh the saved login
and retry the request once. If that still fails, callers receive
`ErrUnauthorized`. Sync treats that as a fatal `auth:account` condition for the
current pass or watch session, and CLI surfaces present it as
`Authentication required`.

This policy is evidence-driven. The repository documents one spurious `401`
class for pre-authenticated upload-session fragment requests when a Bearer
token is sent unnecessarily. That case is handled at the pre-auth transport
boundary by omitting Bearer auth. The repository does **not** currently have
captured evidence for a general post-refresh ordinary-Graph `401` quirk, so no
generic runtime retry or sync-trial behavior exists for that case.

`Client` also exposes an optional per-instance authenticated-success hook. The
hook fires only after a normal authenticated Graph request succeeds. CLI code
uses this as a proof boundary to clear stale `auth:account` scope blocks after
live account or drive commands succeed. Pre-authenticated upload/download URLs
never invoke the hook, so pre-auth transport success is not mistaken for proof
that ordinary Graph auth is healthy.

## Runtime Ownership

The graph package intentionally keeps runtime ownership narrow:

- `Client` holds immutable dependencies and per-instance policy knobs only; it owns no background goroutines, channels, or timers.
- Browser auth owns one callback server goroutine plus one result channel per login attempt. `doAuthCodeLogin` starts them, waits for the callback, and shuts them down before returning.
- Token refresh state is delegated to the token source implementation. `graph` consumes it synchronously; it does not run a background refresh coordinator.
- The sole raw `http.Client.Do` production boundary is `client_preauth.go`, where validated pre-authenticated URLs are dispatched.
- `graphhttp.Provider` owns the long-lived HTTP clients and any caller-scoped shared throttle gates. It has no background goroutines and no package-level mutable state; the provider instance is the runtime owner.

## Design Constraints

- `graph/` exposes concrete types, not interfaces (`graph.Client`, `graph.Item`)
- `graph/` does NOT import `config/` — callers pass token paths directly
- `tokenfile/` is a leaf package (stdlib + oauth2 only)
- Token refresh uses a forked `golang.org/x/oauth2` (`github.com/tonimelisma/oauth2`, branch `on-token-change`) with `Config.OnTokenChange` callback for persistence. Tracks upstream proposal `golang/go#77502`.
- Pre-authenticated URLs (`@microsoft.graph.downloadUrl`, upload session URLs) bypass the Graph API — use `httpClient.Do(req)` directly, no base URL prefix, no auth headers. Never log these URLs.
- The authenticated-success hook is only a proof signal. It must remain optional, per-client, and side-effect free from the graph package's perspective. Graph owns when the hook fires; callers own any resulting state repair.
- `graphhttp.Provider` is the single owner of Graph-facing HTTP client construction. Metadata and transfer clients use transport-level deadlines with `Timeout = 0`; no Graph-facing client uses `http.Client.Timeout`.
- Interactive metadata clients may share 429 `Retry-After` coordination through an injected `retry.ThrottleGate`. The coordination scope is the narrowest CLI-known remote boundary: configured drives use `(account email, driveID)`, configured shared roots use `(account email, driveID, rootItemID)`, and direct shared-item commands use `(account email, remoteDriveID, remoteItemID)`. Broader account-wide or tenant-wide coordination is intentionally not inferred from ordinary file traffic.
- `driveops.SessionProvider` receives a resolver that returns injected metadata/transfer clients per resolved drive. `driveops` stays agnostic about whether those clients came from interactive or sync profiles.
- Upload URLs are sensitive credentials. The `UploadURL` type implements `slog.LogValuer` with redaction, matching the `DownloadURL` pattern.
- Search API calls URL-escape query parameters to prevent special characters from breaking URL construction.
- Token metadata validation is enforced on both write and read paths. Required fields must be present in non-nil metadata. `tokenfile.ValidateMeta()` validates before save, `LoadAndValidate()` validates on load.
- Token file reads and writes use `internal/fsroot` after config-driven token resolution, so open/read/temp-file creation/chmod/fsync/rename all stay inside one managed-state root capability.
- `client_preauth.go` is the sole raw `http.Client.Do` production boundary. Graph base URLs and pre-auth URLs are validated before a request reaches that call; the remaining inline `gosec` suppression there is intentional because the linter cannot prove the validation flow.
- Graph/API quirk handling requires either captured payload evidence in CI/tests/logs or a reproducible documented observation with enough detail to classify safely. One-off incidents without recoverable payload evidence stay documented as reference notes only and do not become permanent runtime normalization rules.
- Broader throttle coordination beyond the proved remote target is deferred until ordinary file traffic provides reliable bucket identity or equally strong evidence for widening. Today the client intentionally blocks only the narrowest known drive/shared boundary. [planned]
- Upload and async-copy pre-auth URL validation: verify HTTPS scheme and trusted Microsoft hosts on upload session and copy monitor URLs before use. Both copy monitor and upload-session validation explicitly allow Personal-account URLs on `microsoftpersonalcontent.com` after live `e2e_full` coverage captured upload-session URLs on that host family. [verified]
- Audit all `slog.*` calls for potential secret leakage (tokens, pre-auth URLs). [verified]
- Audit all error message strings for embedded secrets — `GraphError.Message` and `RawBody` are redacted before exposure. [verified]
- Test that captures log output and verifies no tokens or pre-auth URLs appear. [verified]
- Authenticated request helpers are package-internal (`do` / `doWithHeaders`). External callers use higher-level graph operations instead of raw request dispatch. [verified]
- Monitor `search(q='*')` reliability on business accounts for shared item discovery. [planned]
- `PermanentDeleteItem` 405→`DeleteItem` fallback for Personal accounts is a workaround. Remove when MS adds Personal support.

## Struct Tag Policy

Repo-wide JSON/TOML linting treats CLI and persisted payloads as snake_case by default. `internal/graph` is the deliberate exception: Graph wire structs keep upstream camelCase tags, and annotation-backed fields such as `@odata.nextLink` and `@microsoft.graph.downloadUrl` are exempted in lint configuration rather than suppressed inline.
