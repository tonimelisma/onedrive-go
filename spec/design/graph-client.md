# Graph Client

GOVERNS: internal/graph/auth.go, internal/graph/auth_browser.go, internal/graph/auth_device.go, internal/graph/auth_persist.go, internal/graph/auth_refresh.go, internal/graph/auth_token.go, internal/graph/client.go, internal/graph/client_auth.go, internal/graph/client_construction.go, internal/graph/client_preauth.go, internal/graph/delta.go, internal/graph/download.go, internal/graph/drives.go, internal/graph/drives_identity.go, internal/graph/drives_shared.go, internal/graph/drives_sites.go, internal/graph/errors.go, internal/graph/item_paths.go, internal/graph/item_create.go, internal/graph/items.go, internal/graph/items_copy.go, internal/graph/items_fetch.go, internal/graph/items_mutation.go, internal/graph/items_permissions.go, internal/graph/normalize.go, internal/graph/quirks.go, internal/graph/redaction.go, internal/graph/response_body.go, internal/graph/shares.go, internal/graph/socketio.go, internal/graph/transfer_watchdog.go, internal/graph/types.go, internal/graph/upload.go, internal/graph/upload_session.go, internal/graph/upload_transfer.go, internal/graph/url_validation.go, internal/graphtransport/doc.go, internal/graphtransport/profiles.go, internal/tokenfile/lock.go, internal/tokenfile/tempfile.go, internal/tokenfile/tokenfile.go

Implements: R-3.1 [verified], R-6.7 [implemented], R-6.8 [verified], R-1.1 [verified], R-1.4 [verified], R-1.5 [verified], R-1.6 [verified], R-1.6.2 [verified], R-1.7 [verified], R-1.8 [verified], R-1.2.5 [verified], R-1.3.5 [verified], R-3.6.4 [verified], R-6.7.8 [verified], R-6.7.9 [verified], R-6.7.10 [verified], R-6.7.11 [verified], R-6.7.12 [verified], R-6.7.13 [verified], R-6.7.16 [verified], R-6.7.17 [verified], R-6.7.18 [verified], R-6.7.22 [verified], R-6.7.23 [verified], R-6.7.26 [verified], R-6.8.4 [verified], R-6.8.6 [verified], R-6.8.8 [verified], R-6.8.14 [verified], R-6.3.4 [verified], R-6.8.16 [verified], R-6.10.6 [verified], R-6.8.17 [verified], R-6.8.27 [verified], R-6.7.30 [verified], R-6.7.35 [verified], R-6.8.20 [verified]

## Overview

All Microsoft Graph API communication flows through `graph.Client`, but Graph-facing HTTP runtime policy is split across `internal/graphtransport` and `driveops.SessionRuntime`. The split is deliberate:

- `graph.Client` is a pure API mapper plus a narrow Graph-quirk normalizer: authentication, endpoint-local quirk retries, and error construction.
- `graphtransport` is the stateless home for Graph-facing `http.Transport` / `http.Client` profile construction.
- `driveops.SessionRuntime` is the single runtime owner of long-lived client reuse, token-source caching (one source per sign-in file for every client it builds, drive sessions and account-level bootstrap clients alike, R-6.8.27), and optional shared throttle coordination for retrying metadata clients.
- `retry.RetryTransport` stays generic. It owns transport retry mechanics, not Graph semantics or caller profile selection.

Callers receive clean, consistent data via `graph.Item` and never deal with API inconsistencies. Callers also choose the right HTTP profile without teaching `graph` about CLI-vs-sync policy.

## Graph HTTP Runtime Policy (`internal/graphtransport`)

`internal/graphtransport` constructs the concrete HTTP profiles used for Graph work:

- `BootstrapMetadataClient(logger, notices)`: retrying metadata client for login/bootstrap/account discovery before account identity is known
- `InteractiveMetadataClient(logger, gate, notices)`: retrying metadata client for one interactive target, with caller-owned shared 429 coordination
- `InteractiveTransferClient(logger, notices)`: retrying transfer client for upload/download/copy-monitor flows
- `SyncClientSet()`: non-retrying metadata/transfer clients for sync classification and sync uploads/downloads
- `AuthClient()`: the sign-in service's client (token refresh, device code, code exchange), not Graph-facing; see Authentication below

Every retrying builder takes the caller's shared `*retry.Notices`, so one
outage or long rate-limit wait is announced once per process rather than once
per client (R-6.6.8); a nil `notices` gives the client private announcements.

`driveops.SessionRuntime` composes those stateless builders into the three runtime shapes the app actually uses:

- bootstrap metadata for login/account discovery before target identity is known
- target-scoped interactive metadata + shared interactive transfer clients for ordinary CLI work
- shared non-retrying sync clients for sync workers

Bootstrap remains intentionally unscoped. Before the CLI has both caller
identity and remote target identity, it cannot safely infer a narrower shared
throttle domain, so login/account discovery/share-URL resolution use
the bootstrap metadata client and only switch to target-scoped interactive profiles once
the target is known.

`driveops.SessionRuntime` owns the runtime rule that all Graph-facing clients use `Timeout = 0`. (The auth client is not Graph-facing and is the one exception; see Authentication below.) Stall detection lives in the transport:

- metadata and transfer transports clone `http.DefaultTransport`
- `ResponseHeaderTimeout` detects servers that accept the connection but stall before sending headers
- dial and TLS handshake deadlines protect connection setup
- keepalives detect dead connections without bounding total transfer duration
- after the headers, `graph` bounds every response body's silence (Error Handling)

`graphtransport` also treats `http.DefaultTransport` being anything other than
`*http.Transport` as an invariant violation. The stdlib guarantees the default
shape; a global replacement would silently drop the tuned clone settings, so
the builder panics instead of constructing a degraded fallback transport.

This boundary exists to prevent one blunt client-wide timeout from becoming a competing owner of caller deadlines. Total operation budgets still belong to caller contexts. Transport stall detection up to the response headers belongs to `graphtransport`.

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
| Auth flows, token persistence, and browser/device login remain Graph-boundary responsibilities. | `internal/graph/auth_test.go` (`TestDoLogin_Success`, `TestDoLogin_DeviceAuthError`, `TestSaveLoadToken_RoundTrip`), `internal/graph/auth_browser_callback_test.go` (`TestHandleOAuthCallback_RepeatedCallbackDoesNotBlock`), `internal/graph/client_auth_refresh_test.go` (`TestDeviceLoginError_AADOutcomesArePlain`) |
| Graph request normalization and error translation stay inside the Graph boundary. | `internal/graph/client_test.go` (`TestDo_ErrorClassification`, `TestDo_ParsesStructuredGraphErrors`), `internal/graph/errors_classification_test.go` (`TestIsQuotaExhaustedAndIsNotSupported`), `internal/graph/normalize_test.go` |
| Drive, shared-item, and upload-session quirks are handled at the Graph edge rather than in CLI or sync. | `internal/graph/drives_test.go`, `internal/graph/shares_test.go` (`TestResolveShareURL_PreservesRemoteItemIdentity`), `internal/graph/items_test.go` (`TestToItem_SharedOwner_FallbackChain`), `internal/graph/quirks_test.go` (`TestQueryUploadSession_ReturnsNextExpectedRanges`), `internal/graph/upload_test.go` |
| `@microsoft.graph.downloadUrl` is validated against the content host allow-list before either download route sends a request, the refusal never echoes the URL, and only a loopback base URL (a test fixture) admits loopback pre-auth URLs (R-6.8.22). | `TestDownload_RejectsUntrustedDownloadURL`, `TestValidateDownloadURL`, `TestParseAndValidatePreAuthURL`, `TestNewClient_LoopbackBaseURLAcceptsLoopbackPreAuthURLs` |
| The authenticated request helpers take the body as bytes, and every attempt, the 401 refresh-and-retry included, is built from them: the retry sends exactly the bytes of the first request, and a body that cannot be replayed cannot be passed (R-6.8.14). | `TestDo_401RetrySendsTheBytesOfTheFirstRequest`, `TestDoOnce_401_RefreshSucceedsWithBodyAndQueryPreserved`, `TestDoOnce_401_RefreshSucceeds`, `TestDo_ContentTypeForBody` |
| A request that never left the client (token source failed, a token refresh its context cut short included, context already ended, request not buildable, a retry after a refusal not sent, a throttle wait the retrying transport would not start past the deadline) carries `errRequestNotSent`, and neither the create mark nor the final-fragment mark is set on it (R-5.6.12, R-5.6.15, R-6.8.27). | `TestCreateFile_TokenFailureIsNotOutcomeUnknown`, `TestCreateFile_RequestBuildFailureIsNotOutcomeUnknown`, `TestCreateFile_UnsentRetryIsNotOutcomeUnknown`, `TestCreateFile_MarksOnlyAFailureThatMayHaveCreatedTheFile`, `TestUploadFromSession_UnsentFinalFragmentIsNotUncertain`, `TestDo_ContextCancellation`, `TestDo_ATokenRefreshCutByTheContextIsNotSent`, `TestDo_AThrottleWaitPastTheDeadlineIsNotSent`, `TestSimpleUploadToItem_TokenError` |
| The Socket.IO notification URL is sensitive in both spellings (`/callback` and the `/socket.io` websocket form, or any `snthgk` query), and `*url.Error` redaction runs before wrapping (R-6.8.17). | `TestIsSensitiveURL_SocketIONotificationURLs`, `TestRedactURLError_ScrubsSocketIOWebsocketURL`, `TestRedactURLError_MustRunBeforeWrapping` |
| Item permissions and site search follow `@odata.nextLink` through `stripBaseURL`, bounded by the shared page cap; site search stops at the caller's limit; every paged listing, delta enumeration included, fails on a next link back to a page it already asked for, before asking for it again (R-6.7.32). | `TestListItemPermissions_FollowsNextLinkAcrossPages`, `TestListItemPermissions_NextLinkOutsideBaseURLIsRefused`, `TestSearchSites_FollowsNextLinkAcrossPages`, `TestSearchSites_StopsFollowingAtTheLimit`, `TestPagedListings_ALinkBackToAPageAlreadyAskedForFailsTheListing` |
| Every sign-in request (silent refresh, device code and its polls, code exchange) goes through the injected auth client, whose deadlines (30 s headers, 60 s request) release every caller of a stalled token endpoint; a nil auth client, one without a timeout and a nil Graph HTTP client are errors, never `http.DefaultClient` (R-6.8.27). | `TestAuthClient_BoundsEveryRequest`, `TestTokenSourceFromPath_RefreshUsesTheInjectedClient`, `TestTokenSourceFromPath_StalledTokenEndpointTimesOut`, `TestTokenSourceFromPath_NilAuthClientIsAnError`, `TestSignInFlows_AuthClientWithoutATimeoutIsAnError`, `TestLoginFlows_NilAuthClientIsAnError`, `TestLogin_UsesTheInjectedAuthClient`, `TestLoginWithBrowser_UsesTheInjectedAuthClient`, `TestNewClient_NilHTTPClientIsAnError` |
| The browser sign-in's callback server is owned: shutdown waits for its serving goroutine, a panic there is the login's callback error, and a handler's panic, which net/http recovers per connection, is logged through the login's logger, not the standard `log` package (R-6.3.10, R-6.3.4). | `TestCallbackServer_HandlerPanicIsLoggedAndShutdownWaitsForServe` |
| A token refresh runs under the context of the request that needs it: one refresh at a time, shared by every caller waiting for it, each caller waiting no longer than its own context (whose cause it keeps); a caller that leaves does not cancel it for the others, a refresh nobody waits for ends by the sign-in client's timeout, a panic in it fails its callers, and only a refresh that finished with a token writes the token file (R-6.8.27). | `TestTokenSource_ARefreshEndsWithTheCallersContext`, `TestTokenSource_ARefreshOneCallerAbandonedStillServesTheNext`, `TestTokenSource_AnAbandonedRefreshWritesNoTokenFile`, `TestTokenSource_ConcurrentCallersShareOneRefresh`, `TestTokenSource_ARefreshThatPanicsFailsItsCallers`, `TestTokenBridge_ForceRefreshRedeemsRefreshTokenOncePerBurst` |
| A failed request to the sign-in service (refresh, device code, poll, code exchange) carries `*SignInRequestError` through every wrapping; a Graph request's failure does not (R-6.6.22). | `TestSignInRequestError_MarksEveryRequestToTheSignInService`, `TestSignInRequestError_NotOnAGraphRequest`, `TestCommandErrorText_ASignInFailureNamesMicrosoftSignIn` |
| One sign-in file has one token source per session runtime, loaded once outside the runtime's lock: account-level bootstrap clients and drive sessions share it, and the CLI builds no token source of its own; each sign-in file has its own bootstrap metadata client, so its throttle is its own (R-6.8.27). | `TestSessionRuntime_BootstrapClientReusesOneTokenSourcePerPath`, `TestSessionRuntime_BootstrapClientKeepsNotLoggedIn`, `TestSessionRuntime_DefaultTokenSourceHasTheAuthClient`, `TestSessionRuntime_ATokenLoadDoesNotHoldTheRuntime`, `TestSessionRuntime_ConcurrentCallersOfOneSignInLoadItOnce`, `TestSessionRuntime_OneSignInsThrottleDoesNotReachAnother`, `TestCLIContextSession_ProbeAndSessionShareOneTokenSource`, `TestCLIBootstrapClientsUseTheSessionRuntime` |
| A failed answer's error text names its most specific code, cut and quoted when it is not plain; its debug record names the code chain, the content type and the size and kind of the body (never the body or the message), for a zero-byte simple upload as for every other request; an error body that cannot be read leaves its own record with the answer's status and request ID; an upload fragment's unexpected 2xx answer is recorded without its body; and the codes are sanitized like the message (R-2.10.72, R-6.8.17). | `TestGraphError_ErrorString`, `TestBuildError_DebugLineNamesTheRequest`, `TestBuildError_DebugLineRecordsWhatTheBodyHeld`, `TestBuildError_RecordsAnUnreadableBody`, `TestUploadToItem_RefusedEmptyOverwriteRecordsTheAnswer`, `TestUploadToItem_UnreadableErrorBodyIsRecorded`, `TestUploadChunk_UnexpectedStatusRecordsNoBody`, `TestBuildGraphError_RedactsSecretsInCodes` |

## Authentication (`auth.go`)

Two OAuth2 flows:
- **Device code flow** (default): user enters code on microsoft.com
- **PKCE authorization code flow** (`--browser`): localhost callback with code verifier

Token refresh is automatic for normal token sources. Tokens are stored as JSON
files via `tokenfile` package (strict JSON — rejects unknown fields).
`tokenfile.Load` returns `ErrNotFound` sentinel (never nil,nil). Dry-run sync
uses the same token source as live sync; silent refresh and the token-file
write after it remain part of normal authenticated Graph access.
The device-code boundary intentionally stays narrow: production calls
`oauth2.Config.DeviceAuth` and `DeviceAccessToken` directly, while unit tests
can inject those two calls independently to cover cancel/save/error behavior
without waiting on real polling intervals. One smoke test still runs through
the real oauth polling integration.

**The sign-in HTTP client is injected, never defaulted (R-6.8.27).**
`TokenSourceFromPath`, `Login` and `LoginWithBrowser` take an `*http.Client`
and put it into the context as `oauth2.HTTPClient` (`signInContext`) for each
request to the sign-in service, so the device-code request and its polls, the
code exchange and every later silent refresh of the returned source go
through it. Without it oauth2 used `http.DefaultClient`, which has no
timeout: a token endpoint that accepted the connection and never answered
held the refresh, and every Graph request of the account waiting for it,
until the process stopped. Production passes `graphtransport.AuthClient()`:
30 s for response headers and 60 s for the whole request. Unlike the Graph
profiles it sets `http.Client.Timeout`, because a sign-in answer is a small
JSON body (oauth2 caps it at 1 MiB) and nothing about it legitimately takes
a minute; each device-code poll is one such request, so the deadline never
cuts the polling loop short. A nil client, or one without a `Timeout`, is an
error (`checkAuthClient`): a refresh outlives the callers that stop waiting
for it, and only that timeout ends it then. The client has no retry wrapper;
the token source's caller decides what a failed refresh means
(`TestTokenSourceFromPath_StalledTokenEndpointTimesOut`,
`TestSignInFlows_AuthClientWithoutATimeoutIsAnError`).

**A refresh runs under the context of the request that needs the token
(R-6.8.27).** `TokenSource.Token(ctx)` and `ForceRefresh(ctx)` take the
caller's context, and the source keeps none: `TokenSourceFromPath` takes no
context, and neither do the login flows' returned sources. `tokenBridge`
holds the cached token itself (no `oauth2.ReuseTokenSource`): a valid one is
handed out at once; otherwise the caller joins the refresh under way or
starts one (`tokenRefresh`). Each refresh is one token-endpoint request on a
goroutine the bridge owns, run under `context.WithoutCancel` of the context
of the caller that started it, so it keeps that context's values but not its
cancellation. Every caller waits for it no longer than its own context allows
and then fails with `refreshing access token: <context.Cause>`, which
`doOnce` marks `errRequestNotSent`; the refresh goes on for the other
callers, and when nobody waits any more it still ends, by the sign-in
client's `Timeout`, and its outcome serves the next caller. A failed refresh
keeps the old token, so the next caller refreshes again. The token endpoint's
answer is written to the token file by the refresh goroutine before any
caller is given the token, so only a refresh that finished with a token
writes the token file (see the next paragraph). A panic in the refresh (oauth2 or the sign-in
transport) is recovered at that goroutine's boundary through
`panicguard.Recovered` ("Internal error while refreshing the sign-in", logged
once at Error with its stack), and its callers get the error, which wraps
`panicguard.ErrPanic`; no later caller waits on it, so the next one
refreshes again (R-6.3.10, `TestTokenSource_ARefreshEndsWithTheCallersContext`,
`TestTokenSource_ARefreshOneCallerAbandonedStillServesTheNext`,
`TestTokenSource_AnAbandonedRefreshWritesNoTokenFile`,
`TestTokenSource_ConcurrentCallersShareOneRefresh`,
`TestTokenSource_ARefreshThatPanicsFailsItsCallers`). A command's budget
(`status`) therefore covers the refresh, and a cleanup that runs on a context
detached from a canceled command (`replaceDestination`'s restore) can still
refresh.

**A refreshed sign-in that cannot be saved is an error its owner hears
(R-6.8.26).** The build uses the upstream `golang.org/x/oauth2` module; every
token a refresh produces, the expiry-driven one of `Token(ctx)` and the one a
401 forces through `ForceRefresh(ctx)` alike, is written by one saver per
token file (`tokenSaver`, `auth_persist.go`) on the bridge's refresh
goroutine. The saver writes atomically through `tokenfile.Save`. Which
sign-in is newer is decided by lineage, never by expiry, because access-token
lifetimes vary: the sign-in service answers a refresh token with its
successor and refuses the one it replaced, so a token is newer than the file
exactly when it descends from the refresh token the file holds. The saver
remembers that refresh token, and for an unsaved chain the one the file held
when the chain began; it writes a token only while the file still holds the
start of its chain, also when the new token expires first (a forced refresh
soon after a refresh). A file holding anything else was written by another
process and is newer: the unsaved chain is dropped, and a retry never writes
over it (`TestForceRefresh_ShorterLifetimeIsStillSaved`,
`TestPersistingTokenSource_RetryDoesNotOverwriteANewerFile`). A write that fails does not fail the refresh, since the token is in
memory and the requests go on; the newest token stays pending and is written
again at the next token use, and a write that succeeds clears the failure and
logs the one Info line `refreshed sign-in` with the file. The failure is the
bridge's `SaveError()`, read through `graph.TokenSaveError(ts)` (a source
without it, a test fake, reports none); it wraps the exported
`graph.ErrTokenNotSaved` and reads "couldn't save the refreshed sign-in to
<file>; the next run may ask you to sign in again: <cause>", with no token
material. `driveops.SessionRuntime.TokenSaveFailures()` lists the failures of
the sources the runtime holds, by file. A one-shot command hears it once its
work is done: main asks every runtime the command built
(`CLIContext.newSessionRuntime`) and the failure becomes the command's error,
exit status 1, or joins a failure the command already had
(`withTokenSaveFailure`). A watch reads it on its periodic reconcile tick:
one Error per distinct failure, the systemd status names the file while it is
set, and the drives keep syncing; a watch stopped while a save is failing
exits 1 instead of 0 (`TestPersistingTokenSource_SavesARefreshedToken`,
`TestPersistingTokenSource_SaveFailureIsReportedAndRetried`,
`TestPersistingTokenSource_NeverWritesAnOlderToken`,
`TestTokenBridge_ForceRefreshIsSaved`,
`TestTokenBridge_TokenUnderACallersDeadlineIsSaved`,
`TestSessionRuntime_TokenSaveErrors`,
`TestCLIContext_TokenSaveFailureIsTheCommandsError`,
`TestRunWatch_UnsavedSignInIsLoggedOncePerDistinctFailure`,
`TestSyncNotifier_StatusNamesAnUnsavedSignInWhileItIsSet`).

**A failed sign-in request is marked as the sign-in service's (R-6.6.22).**
Every request to the identity platform that fails, the token refresh, the
device-code request, its polls and the browser sign-in's code exchange, has
its error wrapped once in `*graph.SignInRequestError` by one function
(`signInFailure`). The wrapper keeps the request's own error in the chain
(`*url.Error`, `*oauth2.RetrieveError`, a context's error), and it survives
the refresh's `refreshing access token:` wrapping and `doOnce`'s
`errRequestNotSent`, so a caller asks for it with `errors.As`, as for
`*graph.ResponseReadError`, without comparing host names. The CLI names the
failed service from it: "Microsoft sign-in did not answer in time" rather
than "OneDrive did not answer in time" (`cli.md`). A Graph request's own
failure is never marked, and an answer the sign-in service gave (an expired
or refused sign-in) is classified as before
(`TestSignInRequestError_MarksEveryRequestToTheSignInService`,
`TestSignInRequestError_NotOnAGraphRequest`).

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
- Paginated non-delta item surfaces (`ListChildrenPages` and the slice forms
  built on it, drive search) keep
  provider-visible package items. Sync-only package suppression is owned by
  `internal/sync/item_converter.go`, not by the Graph client.
- An item's kind is reported, never defaulted: `Item.IsBundle` decodes the
  `bundle` facet (albums), and `Item.TypeFacetMissing` marks a non-deleted
  item with none of the file, folder, root, package, bundle or remoteItem
  facets, which sparse delta updates can be. Callers decide what an unknown
  kind means (`internal/sync` keeps the baseline kind, R-2.4.16);
  `TestToItem_BundleAndMissingTypeFacet`.
- Delta deletion reordering (deletions before creations within each page)
- Missing field recovery (name, size for deleted items)
- Timestamp validation that preserves unknown timestamps as zero time instead of fabricating a replacement, including `null`, missing, invalid, and out-of-range wire values
- `parentReference.path` is usually unavailable in delta and sync primarily
  tracks remote items by ID. When Graph does provide a valid path, it is
  kept verbatim (it is not URL-encoded, L-32) as root-relative `Item.ParentPath`; `Item.ParentPathKnown`
  distinguishes a known root parent from an omitted/invalid path because both
  normalize to an empty string.
- A read by path or by name is held to its place, as far as OneDrive's answers tell, by four rules; (2) names a case in which answers that agree with one another still name the wrong place. (1) A name the path route cannot carry is refused before any request: `refuseUnaddressableNames`, which `GetItemByPath`, `ListChildrenByPathPages` and `GetChildByName` call first, makes a path or a name holding `:` in any of its names `ErrNameNotPathAddressable` (the colon ends the path in the route, measured on a Personal drive, `graph-api-quirks.md`), and `GetChildByName` also refuses a name escaped longer than `maxPathAddressedLength` (`TestPathReads_RefuseAColonInAnyNameBeforeAnyRequest`, `TestGetChildByName_RefusesANameTheRouteCannotCarry`). (2) An answer the answers put elsewhere is refused, and answers that do not agree on the folder at a path are not taken for an absent item. `GetItemByPath` places its answer (`readPlacedAtPath`): one that carries its parent path (`parentReference.path` that survived normalization) by comparing it with the asked path by `namekey.EqualPath`; one without must carry the asked leaf name and sit in the folder that holds the path by `itemSitsIn` (`placeByParent`): for a top-level name the drive root, read by its alias, since an answer without a parent path names the root only by its own ID; otherwise the folder read once by its own path and held to it as far as its answer allows (`readItemAtPath`: by its parent path, or by its name when it carries none), and not placed by its own parent in turn. The answers put the item elsewhere, which is `ErrNotFound`, when its own parent path or name is another place's, when a top-level item sits outside the drive root, when the folder it sits in is answered at another path or under another name, or when another folder is answered at the parent's path by its own parent path. They do not show whether it is at the path when the read of the parent's path finds nothing, or answers another folder that is held there by its name alone or is itself at another place: that is `ErrPathAnswersDisagree`, which is not `ErrNotFound`, since a caller that takes `ErrNotFound` as leave to look elsewhere (a delete's fallback to the parent's listing, a create at the path) would act on one of the answers. An answer below the root that names no folder cannot be placed, and is an error too. Such answers can settle as OneDrive converges after a change, so `driveops.MountSession.WaitPathVisible` reads the path again on `ErrPathAnswersDisagree` as on `ErrNotFound`, without listing a folder because of it, while a delete's target is refused at once. A read by path costs one request for each chunk of the path when its answer carries its parent path, and at most twice that when it does not, at any depth (a path is one chunk unless its escaped form is longer than `maxPathAddressedLength`); placing the parent by its own parent too cost one request more for each folder above it, on every read of a drive whose answers carry no parent path. The bound has a price on such a drive: where every answer agrees on a folder of the right name in the wrong place two or more levels up (the path route resolving the path into another folder's subtree), the read returns that folder's item, as the check of the leaf name alone before it did. Measured on a Personal drive, answers to nested reads by path carried their parent path, which the exact comparison holds; whether Business and SharePoint answers carry it is unknown, and whether the children of a listing by path carry it is unmeasured. That extra read happens only on this path, which the client logs at Debug. A listing by path is held the same way, since its route resolves the path as a read's does: a path too long for one request is listed by `ListChildrenByPathPages` only after `GetItemByPath` held its folder to the path, and a shorter one holds each page to the path before handing it on (`heldToListedPath`). A child that carries its parent path must name the asked folder, and one that names another is `ErrNotFound`, as a read's answer from another path is; one that does not must sit in the folder the read of the path answers, read once for the whole listing, placed as `GetItemByPath` places it, and only when such a child appears. A child of that folder when the answers put it at another place, or of another folder than the one the answers' own parent paths and IDs put at the path, is not in the folder at the path either: `ErrNotFound`. A child of another folder than one held at the path by a name alone or answered at another place, and a read of the path that finds nothing, are `ErrPathAnswersDisagree`; a child that names no folder is an error too. So a caller that takes an item from the listing (a delete's or a visibility wait's fallback in `driveops`) takes none that the answers place in another folder, and none where they disagree; where neither the children nor the folder read carry a parent path and they agree on a folder in another subtree, the case above applies (`TestGetItemByPath_FuzzyParentMismatchReturnsNotFound`, `TestGetItemByPath_AnswerWithoutParentPathIsPlacedByItsParentsID`, `TestGetItemByPath_AShortPathCostsAtMostTwoRequestsAtAnyDepth`, `TestGetItemByPath_TheParentThatPlacesAnAnswerIsHeldToItsPath`, `TestGetItemByPath_AgreeingAnswersFromAnotherSubtreeAreNotCaught`, `TestListChildrenByPath_LongPathListsNoFolderAnsweredFromAnotherPath`, `TestListChildrenByPath_HandsOnOnlyChildrenItsAnswersPlaceAtThePath`, `TestMountSession_AFallbackTakesNoItemFromAListingThatNamesAnotherFolder`, `TestMountSession_ADeleteTargetIsNotTakenWhereAnswersNameDifferentFolders`, `TestMountSession_AVisibilityWaitReadsAgainWhileAnswersNameDifferentFolders`). (3) An item ID goes into every route escaped as one path segment by one helper, `itemRoute`, as a name is: a Personal item ID holds `!` (`TestItemRoutes_EscapeTheItemIDAsOnePathSegment`). The drive ID goes in as it is. (4) A lookup of one name lists its folder only when one read cannot answer: a create answered without its item reads the file back by `GetChildByName` and lists its folder, a page at a time up to the page that holds the name as spelled, only when that read cannot settle it (`readCannotSettleName`: not found, not addressable, or 400), as `driveops.FindExistingChild` does for its callers; a page that holds only another spelling OneDrive treats as the same does not stop the listing, as a later page could hold the exact one, and a folder of the name is not the created file (`TestCreateFile_ReadsTheCreatedFileBackByItsName`, `TestCreateFile_TheReadBackPrefersTheNameAsSpelled`, `TestCreateFile_AFolderOfTheNameIsNotTheCreatedFile`, `TestLookupUploadTarget_ReadsTheNameAndListsOnlyWhenTheReadCannotSettleIt`, `TestMountSession_ResolveItem_MountRootReadsEachNameOnce`). The one exception is the read-back of a folder whose create answered an empty body, which lists the parent on each attempt (`readCreatedFolderAfterEmptySuccessBody`).
- `GetChildByName` reads one name under a parent ID (`items/{parent}:/{name}:`, the route `fetchItemByPathChunks` sends for each further chunk, built in one place, `childPathRoute`) and accepts the answer only when it sits directly in that parent (its parent ID compared without letter case, as item IDs carry a drive ID prefix in either case; for the drive root's alias `root`, which no answer carries, when its `parentReference.path` is the top of the drive and its `parentReference.driveId`, when present, is the asked drive) and its name equals the asked name by `namekey.Equal`; anything else is `ErrNotFound`. A name the route cannot carry is `ErrNameNotPathAddressable` before any request (rule (1) above). A read Graph answers 400 is `ErrBadRequest`, as any request's 400 is: measured for a child named `$value` that the parent's listing holds (`graph-api-quirks.md`), so `driveops.FindExistingChild` lists the parent on a 400 as on `ErrNotFound` and `ErrNameNotPathAddressable` (R-6.7.8, R-6.7.22, `TestGetChildByName_SendsTheNameAsOneEscapedSegment`, `TestGetChildByName_AcceptsTheStoredSpellingOfTheSameName`, `TestGetChildByName_RefusesAnAnswerThatIsNotTheAskedName`, `TestGetChildByName_PlacesAnAnswerForTheRootAliasByItsParentPath`, `TestGetChildByName_RefusesANameTheRouteCannotCarry`).
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
- `maxListPages`: upper bound on the pages every non-delta paged listing
  asks for (`defaultMaxListPages`; tests lower it)
- full delta enumeration fails closed when a provider page has neither
  `@odata.nextLink` nor `@odata.deltaLink`; callers must never receive an
  empty completed cursor after malformed provider pagination
- `maxRecursionDepth`: upper bound for recursive child listing
- `driveDiscoveryPolicy`: transient 403 retry policy for `/me/drives`
- `rootChildrenPolicy`: transient 404 retry policy for `root/children`
- `downloadMetadataPolicy`: transient 404 retry policy for item-by-ID download metadata lookup
- `fileCreatePolicy`: transient 404 retry policy for the create of a new file (`CreateFile`) under a freshly created parent
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

The graph boundary validates the returned notification URL before exposing it to callers. Accepted production endpoints must be HTTPS on Microsoft `*.svc.ms` hosts. The client also redacts `notificationUrl` values from structured Graph errors and plain-text URL scrubbing. `isSensitiveURL` recognizes the URL in both spellings — the `/callback` form Graph returns and the `wss://…/socket.io/?…&snthgk=…` form observation dials — and `snthgk`, the query parameter that carries its credential, is on the sensitive-key list, so the URL is scrubbed wherever it appears in error text (`TestIsSensitiveURL_SocketIONotificationURLs`). The dial itself happens in observation, which drops the URL from a failed dial before logging it (see [sync-observation.md](sync-observation.md)), because `redactURLError` cannot reach a `*url.Error` once `fmt.Errorf` has wrapped it (`TestRedactURLError_MustRunBeforeWrapping`).

The Graph client does not own websocket runtime state. It performs one synchronous endpoint fetch and returns validated data; connection lifecycle, Engine.IO / Socket.IO framing, ping/pong, reconnect, and renewal are observation-layer responsibilities. The returned `notificationUrl` is the raw Graph callback URL, not the final RFC6455 websocket URL — observation is responsible for the `/socket.io/?EIO=4&transport=websocket` transform described in the reference layer.

## Item Operations (`items.go`)

GetItem, ListChildren and ListChildrenPages (see Paged Listings), CreateFolder, CreateFile, MoveItem, CopyItem, DeleteItem. Every check of whether a returned name is the one asked for (`GetItemByPath` verification, created-item read-backs) uses `internal/namekey`, OneDrive's own name equivalence, not `strings.EqualFold` (R-2.12.3, `TestGetItemByPath_PathValidationUsesNamespaceKey`). A read-back that picks the created item from its folder's listing (`findCreatedFolder` for an empty create answer, `NameSeek` for a file the read of its name cannot settle) applies `namekey.Find`: the child spelled as asked, else the only child of a name OneDrive treats as the same; two such children and none spelled so are not taken, and a child the pages repeat counts once (`WithoutRepeats`; `TestFindCreatedFolder_TwoFoldedFoldersAreNotFound`, `TestReadCreatedFileByName_PrefersExactSpelling`). A file read-back that finds two answers `ErrCreateOutcomeUnknown` and says so. All operations use `graph.Item` — the clean type after normalization. `Item.ParentPath` carries the decoded root-relative `parentReference.path` when Graph provides it and `Item.ParentPathKnown` records that the path was valid, so callers never need to parse Graph's absolute `"/drives/{id}/root:..."` representation themselves or guess whether an empty parent path means root or unknown. `MoveItemIfMatch` and `DeleteItemIfMatch` are the conditional mutation variants used by sync execution after live preflight; they add `If-Match` when the caller supplies an eTag and map HTTP 412 to `ErrPreconditionFailed`.

`CreateFolder` also owns one narrow ambiguous-success recovery path. If Graph
returns a success status for `POST .../children` but the body is empty, the
client does not resend the create. Instead it confirms the new folder by
re-listing the parent collection under the same bounded post-mutation
visibility budget, because a replay after a success it already has would turn
that success into a `nameAlreadyExists` conflict. (A transient failure is
different: there the outcome is unknown, and the retrying transport resends
the create, whose 409 the caller resolves by looking the name up; R-6.8.21.)

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

- `download.go`: streaming download with content URL. `@microsoft.graph.downloadUrl` is server-supplied, so both routes pass it through `validatedDownloadURL` — HTTPS, no IP literal, a host on the content allow-list shared with upload sessions (`preAuthContentHosts`) — before building the request, and the refusal never echoes the URL (R-6.8.22, `TestDownload_RejectsUntrustedDownloadURL`). A ranged read is bound to a `DownloadVersion` and accepted only as a 206 whose Content-Range starts at the offset and spans the item (`validateContentRange`, `ErrRangeResumeRejected`); for a hashless item, an HTML or JSON answer of another type whose length is not the item's is `ErrUnexpectedDownloadBody` (`checkDownloadBodyType`); hashed items are never gated by type, since the hash decides
- `item_create.go`: new files. `CreateFile` posts the name in a JSON body with `conflictBehavior=fail`; `Upload` and `CreateUploadSession` then send the content by item ID through a session conditional on the created eTag, and a zero-byte file is the created item alone (R-5.6.14, L-01). No name ever goes into a URL: a path route turned `v1.0` into folder `_api` holding `v2.0` (probe 4). A failed `CreateFile` that does not say whether the file was created carries `ErrCreateOutcomeUnknown`: no response, a context that ended while the request was out, a 5xx, a 408, and a 2xx whose item could not be decoded or read back. A definite refusal (409, 429, any other 4xx) does not, and neither does a create that never left the client (`errRequestNotSent`: the token source failed, the context had already ended, the request could not be built, or the retry after a 401 or a fresh parent's 404 was never sent; every attempt the graph client itself made before it was a refusal). Only the answers the client sees are judged: a retrying transport's own attempts inside one dispatch are not, and because the create is marked replay safe, under the CLI's transport a 5xx attempt can sit unseen before the last refusal. Sync, the only reader of the mark, uses a transport that retries nothing (R-6.8.8), so the sync executor keeps its create intent for exactly the first kind (R-5.6.15, `TestCreateFile_MarksOnlyAFailureThatMayHaveCreatedTheFile`, `TestCreateFile_TokenFailureIsNotOutcomeUnknown`, `TestCreateFile_RequestBuildFailureIsNotOutcomeUnknown`, `TestCreateFile_UnsentRetryIsNotOutcomeUnknown`)
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
`defaultStallTimeout` (2 minutes, the window of every response body too; tests
shorten `stallTimeout`). It is armed only around body I/O, while a request
body is being written and while a response body is read; the wait for
headers belongs to the transports' `ResponseHeaderTimeout`, and
`RetryTransport` disarms it before every retry or throttle wait
(`retry.WithBackoffObserver`), so a long `Retry-After` is not a stall
(`TestUploadChunk_RetryAfterLongerThanStallTimeoutIsNotAStall`). A body
reader belongs to one attempt. The transport reads a request body on its own
goroutine, and can still be inside an abandoned attempt's body when the
retry wait starts, while the next attempt runs and after the call has
returned, so the watchdog decides "is this reader's attempt still the
current one" and changes the timer as one step under its lock; a wait and
the end of the transfer both retire every reader handed out so far. With the
check and the timer change apart, a read armed the timer just after the wait
had stopped it, and a `Retry-After` longer than the stall timeout failed the
fragment as stalled
(`TestStallWatchdog_AWaitThatStartsDuringAReadLeavesTheTimerStopped`,
`TestStallWatchdog_AnAbandonedAttemptsReadCannotDisarmTheNextAttempt`,
`TestStallWatchdog_NothingArmsItOnceTheTransferHasEnded`). A stall is
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
`TestUploadChunk_StalledBodyIsAbandoned`). A download's response body comes
back bounded like every response body (Error Handling below); handed to the
watchdog's `reader`, it starts no timer of its own, so the watchdog alone
bounds it: the windows are the same, and a download failed as a stalled
response instead of a stalled transfer would lose its partial
(`TestStallWatchdog_ABodyItReadsStartsNoTimerOfItsOwn`). The transfer and metadata
transports also ping idle HTTP/2 connections (`graphtransport`,
`TestTransferTransport_PingsIdleHTTP2Connections`). The stall timer's
callback (`stalled`) recovers its own panic and cancels the transfer with the
panic's error, which `explain` returns with the transfer's error kept as
text, so that one transfer fails and nothing else (R-6.3.10,
`TestStallWatchdog_ACallbackPanicFailsTheTransferWithIt`).

Mutation answers go through `decodeMutatedItem`: a 2xx whose body is empty
(204), not a driveItem (a 201 carrying `{"error":…}`), or an item without
`id` is read back by ID instead of failing as a JSON "EOF" decode error that
classified a mutation that had succeeded as retry work (D-047, R-6.7.33,
`TestMutationResponsesWithoutItem`). An upload completion without its item
returns `ErrUploadCompletedWithoutItem`, because a create has no ID to read
back; the transfer layer looks it up. Items carry `parentReference.driveType`
as `ParentDriveType`. `MoveItemIfMatch` still decodes strictly; its echo
check belongs with the move-outcome work. Moves take `MoveOptions`
(`IfMatch`, `KeepModified`): a rename otherwise resets the item's modification
time, so callers pass the time they already know and it rides in the same
PATCH (L-09, D-150).

Every non-empty upload, create or overwrite, goes through an upload session
whose creation carries `fileSystemInfo`, so content and mtime land in one
version; zero-byte files are the created item, or a simple PUT overwrite, plus
an mtime PATCH (D-034, live probe 7). The create by body retries the exact
fresh-parent `404 itemNotFound` with `FileCreatePolicy()`; a
read-only shared folder answers it with the correct `403`, which is not
retried. A create never replaces: 409 `nameAlreadyExists` is `ErrConflict`
(`TestUpload_ExistingNameIsNeverReplaced`). When the content step fails
before its final fragment the created empty item is removed again by
`RemoveEmptyItem`: it is read back and deleted while still empty, with
`If-Match` on the eTag just read, because an upload session opened on the
empty item moves its eTag although no content landed
(`TestUpload_FailedContentStepRemovesItsEmptyItemConditionally`,
`TestUpload_FailedContentStepRemovesItsEmptyItemByItsCurrentETag`). One read
and one delete are not enough: a delete answered 412 means the eTag moved
again after the read (LI-20261110-01: a session request the client has
canceled still lands, up to 0.65 s later in a live probe), so the read and the delete are
repeated, `placeholderCleanupAttempts` (3) attempts in all, on a context
detached from the upload's and bounded as a whole by
`placeholderCleanupTimeout` (10 s). The attempts follow one another without
a pause: a live probe on Personal (2026-11-12) saw the eTag move once, when
the session opened or a canceled session request landed, and then hold for a
minute, and a delete conditional on the eTag read last succeeded every time
([graph-api-quirks.md](../reference/graph-api-quirks.md), "An Upload Session
Moves An Empty Item's eTag"). The session itself is canceled first, bounded
by `SessionCancelTimeout` (10 s), the one bound for that request, which the
transfer manager uses too; the cancel is best effort and the bound is not
sized for a provider that refuses slowly. An item that reads back with content or
without an eTag is never deleted; a 404 on either request counts as removed;
any other failure ends the removal. Whatever remains is left in place for the
next pass (R-5.6.14, `TestUpload_FailedContentStepReadsAgainWhenItsDeleteIsRefused`,
`TestUpload_FailedContentStepLeavesAnItemThatGainedContentBetweenAttempts`,
`TestUpload_FailedContentStepStopsAfterThreeRefusedDeletes`,
`TestUpload_FailedContentStepStopsWhenItsEmptyItemIsAlreadyGone`,
`TestRemoveEmptyItem`; live, in the integration tier,
`TestIntegration_RemoveEmptyItem_AfterAbandonedUploadSession`). The failure's
`CreatedItemError.Removed` says whether the removal worked, so sync ends its
record of an item that can no longer receive the content (R-5.6.15,
`TestUpload_FailedContentStepReportsWhetherItRemovedItsEmptyItem`). An upload
that completed without returning its item (a fragment before the last
answered 200 or 201 without a body, `ErrUploadCompletedWithoutItem`) is never
handed to the removal: the item holds the content, and a read-back while
OneDrive still processes it can show it empty
(`TestUpload_CompletedUploadIsNotRemovedAsAPlaceholder`).

Only a last fragment that was sent can have committed the file, so only its
failure is a `FinalFragmentError` (the caller looks the item up before
uploading again). One that never left the client (`errRequestNotSent`: the
session URL was refused before the request was built, or the context ended
after the fragment before it) is a plain failure, and the placeholder of a
create is removed as for any other content-step failure
(`TestUploadFromSession_UnsentFinalFragmentIsNotUncertain`,
`TestUploadFromSession_OnlyFinalFragmentFailureIsUncertain`).

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

A listing that stopped at its first page would fail silently in the worst
way: the caller would receive a short list that looks complete, so a drive
past the first page of `/me/drives` or `/sites/{id}/drives` would not exist
as far as the rest of the program is concerned, a write grant on a second
permissions page would go unseen, and a site past the first search page
would vanish. Every listing's response type declares the continuation
field, and every listing follows it (`TestDrives_FollowsNextLinkAcrossPages`,
`TestSiteDrives_FollowsNextLinkAcrossPages`,
`TestListItemPermissions_FollowsNextLinkAcrossPages`,
`TestSearchSites_FollowsNextLinkAcrossPages`).

The bound matters for the opposite reason: a continuation that never ends
would loop until the process died. Every listing counts the pages it asks for
in one `askedPages`, delta enumeration against `maxDeltaPages` and every other
listing against one cap, `maxListPages`: a listing of exactly that many pages
completes, and a page past it is never asked for
(`TestPagedListings_ThePageCapCountsThePagesAskedFor`,
`TestDeltaListings_AListingOfExactlyThePageCapCompletes`, `TestDeltaAll_MaxPages`,
`TestDeltaFolderAll_MaxPages`).

Every continuation goes through `stripBaseURL`, which refuses a link off the
client's base URL, so a hostile `@odata.nextLink` cannot redirect the bearer
elsewhere (`TestListItemPermissions_NextLinkOutsideBaseURLIsRefused`). A delta
enumeration refuses such a link before it counts or asks for the page, so its
caller is told of the bad link even when the page cap is spent
(`TestDeltaListings_ANextLinkOffTheBaseURLIsRefusedBeforeItIsAsked`).

`SearchSites` takes a caller-supplied limit. Graph treats the `$top` it is
sent as a page size, not a result cap, so the walk follows `@odata.nextLink`
until that many sites are in hand or the pages run out, and never returns
more than the limit (`TestSearchSites_StopsFollowingAtTheLimit`).

A folder's children are read through `ListChildrenPages` (by item ID) or
`ListChildrenByPathPages`. One loop, `visitChildPages`, hands each page to a
`ChildrenPageFunc` as it arrives, in the order OneDrive returns it, and asks
for the next page only after that function returned, so a caller that walks
holds one page (at most 200 children) of a folder of any size. Returning
false stops the listing with no further request, and a context that ends
between pages ends it with the context's error. Stopping early is the one
exception to following `@odata.nextLink` to completion, and only a recursive
`get` takes it: when it cannot create the local folder a listing fills, and
when its context ends the walk. A failed page is retried as itself, by the
retrying transport and, for a page asked for at the drive root's children
collection (the first page, and a later one whose link keeps that form), by
the root 404 quirk retry: the pages before it are neither asked for again nor
handed on twice, and the caller sees the page's error after them.
`ListChildren`, `ListChildrenByPath` and `ListRecycleBinItems` are that loop
appending every page, for callers that need the whole list: a sort or one
output document (`ls`), a lookup by name (the created-item read-backs, the
lookup after a 409, path resolution), or everything at once (sync,
`ListChildrenRecursive`). A caller that only walks the children, a recursive
`get` and the `--force` leftover hint, uses the paged form
(`TestListChildrenPages_HandsEachPageOnBeforeAskingForTheNext`,
`TestListChildrenPages_RetriesAFailedPageAsThatPage`,
`TestListChildrenPages_CancellationBetweenPagesAsksForNoFurtherPage`,
`TestListChildrenPages_ACallerThatStopsAsksForNoFurtherPage`,
`TestListChildren_ReturnsEveryPageInListingOrder`).

The page cap alone does not bound what a circular continuation costs. A next
link back to a page already read would be followed until the cap: a walker
would be handed the same children again, and a recursive `get` would queue
their files and walk their subfolders again each time; a listing that
gathers every page would ask for and hold the cap's pages; a site search,
which stops at its caller's limit, would return a site twice; a delta
enumeration, which follows next links until a delta link comes, can never
finish on one. So `askedPages` also keeps the path of every page its listing
asked for, at most the cap's, and a next link back to one of them fails every
listing with `graph: <listing> looped back from page N to page M` before that page
is asked for again: the slice forms return nothing, a delta enumeration no
token either, and a walker has been handed each page once. A delta
enumeration keys a page on the path its token resolves to: a link's path and
query, and the listing's initial path for a token that is not a link, the
empty one included (`TestResolveDeltaToken`). So a link back to
its first request, its own path or the saved token it started from, is a
repeat as well; the repeat wraps no sentinel, like the cap's error, so its
callers classify the two alike. A site search that reaches its limit before
the link repeats returns its sites
(`TestPagedListings_ALinkBackToAPageAlreadyAskedForFailsTheListing`,
`TestListChildrenPages_ALinkBackToAPageAlreadyAskedForFailsTheListing`,
`TestSearchSites_StopsFollowingAtTheLimit`,
`TestDeltaListings_ALoopBackFailsAsThePageCapDoes`).

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

`GraphError` preserves Graph's structured error metadata: `Code`, `InnerCodes`, capped `RawBody`, and helper methods `MostSpecificCode()` / `HasCode()`. The codes, `Message` and `RawBody` are sanitized when `buildGraphError` builds the error, so bearer tokens and pre-authenticated URLs are redacted even when Graph echoes them back in an error payload, and everything that prints or logs them (`Error()`, the debug record, quirk retry evidence, `MostSpecificErrorCode`) sees only the sanitized copy (`TestBuildGraphError_RedactsSecretsInCodes`). Quirk retries key on the code chain first. One-off incident signatures without recoverable payload evidence stay in the reference layer and are not special-cased in runtime classification.

**Error text** — `Error()` is `graph: HTTP <status> <code> (request-id: <id>): <message>`, where `<code>` is the most specific code; without a code the text has no code part, and without a request ID no request-id part (`graph: HTTP 400: Invalid request`). In the text alone the code is cut to 128 bytes (`maxErrorTextCodeBytes`) and then quoted with `%q` when it holds any byte but an ASCII letter, a digit, `_`, `.` or `-`, so a code from the server can neither break the line nor read as the text's own separators; `Code`, `InnerCodes`, `MostSpecificCode()`, `HasCode()` and the debug record keep the sanitized code as it came, and every code the client compares is plain, so its text is unchanged. When the answer has no `error.message` (a body without a Graph error object, or an error object without a message), the message is the body itself, so a text without a code does not say whether the body was a Graph error object; the debug record's `body_kind` does (`TestGraphError_ErrorString`).

**Debug record of a failed answer** — `buildError` builds the error of a non-2xx answer to an authenticated request (`doRequest`) or a pre-authenticated one (`doPreAuth`), and `rawUploadError` that of the zero-byte simple upload (`doRawUpload`); both log the answer once at Debug through `logFailedAnswer` as `request failed` with `method`, `path` (never a pre-authenticated URL), `status`, `request_id` and `ags_diagnostic`, and the answer's payload evidence: the outer `code`, the `inner_codes` in order (an empty list when none), the answer's `content_type`, `body_bytes` (what arrived, up to `maxErrBodySize`) and `body_kind`, which is `graph_error` for an `error` member naming a code, a message or an inner code, `other` for any other body (plain text, HTML, JSON that is not a Graph error object) and `empty` for none. It never holds the body or the message. A `body_bytes` of 65536, the bound, means the body was cut there unless it was exactly that long, and a cut JSON body no longer decodes, so it reads as `other` without a code. The record is written once per answer returned to the caller: an attempt the retrying transport absorbs and resends is logged by the transport as `retrying after HTTP error` with its status only; a first 401 leaves no record when the request is sent again with another token, and when nothing more is sent because the token cannot be refreshed, the 401's body cannot be closed or the second request cannot be built; it is the recorded answer when no other token can be had; each attempt of a documented quirk retry is a request of its own and leaves its own record. An error body that cannot be read leaves a record of its own, `failed to read response body`, with `method`, `path`, `status`, `request_id` and the read error; `buildError` then records the answer with the bytes that arrived, naming the codes of a whole error object that came before the failure, and returns a `*GraphError` with a placeholder message and no code, while the simple upload returns a plain error (`graph: reading upload error body: ...`) and leaves no `request failed` record. An upload fragment's unexpected 2xx answer is recorded as `PUT` `preauth:upload chunk` with its `status`, `request_id`, `body_bytes` and `body_kind`, never its body, and one whose body cannot be read by the record of an unreadable body under the same name (`TestBuildError_DebugLineNamesTheRequest`, `TestBuildError_DebugLineRecordsWhatTheBodyHeld`, `TestBuildError_RecordsAnUnreadableBody`, `TestUploadToItem_RefusedEmptyOverwriteRecordsTheAnswer`, `TestUploadToItem_UnreadableErrorBodyIsRecorded`, `TestUploadChunk_UnexpectedStatusRecordsNoBody`, `TestUploadChunk_UnreadableUnexpectedStatusBodyIsRecorded`). This record is the payload evidence the Design Constraints ask of a quirk rule ([graph-api-quirks.md](../reference/graph-api-quirks.md), "A Write Can Be Refused With A Bare 400").

Bounded documented-quirk retry exhaustion is surfaced as `QuirkRetryError`.
That wrapper is part of the same wire-to-domain boundary: it adds quirk-level
attempt evidence for caller logs and verifier output, but it does not create a
second error classification scheme. Callers still branch on the unwrapped
terminal cause (`ErrForbidden`, `ErrNotFound`, `*GraphError`, and so on), then
optionally project the attached retry evidence through `ExtractQuirkEvidence`
when they intentionally degrade or summarize verifier output.

Each documented quirk has an exported name constant (`graph.Quirk*`, for
example `QuirkFileCreateTransient404`). That name is the stable
identifier: it is `QuirkRetryError.Quirk`, and it is the `graph.QuirkLogKey`
(`graph_quirk`) field on the one debug record logged when a budget runs out
(per-attempt records use `quirk`). Code and tests that must recognize a quirk
use `errors.As` in process or that log field across a process boundary
(the live-test fixture classifier reads it from `--debug` stderr), never the
error text. `QuirkRetryError.Error()` is plain words for the symptom ("OneDrive
kept reporting the new file's folder as missing, even after 7
tries: ...") and deliberately does not contain the quirk name, so rewording it
cannot break a classifier.

`dispatchRequestWith`, the one place every response that reaches the client
comes back through, wraps every response body (`responseBody`), so a read
failure other than `io.EOF` carries `*ResponseReadError` (cause reachable
through `Unwrap`). It tells "the connection dropped while OneDrive was
answering" apart from a local write failure returned by the same `io.Copy`,
which net/http cannot: it reports body read failures without the `*url.Error`
it uses before the response. The wrap also bounds the body's silence, so no
call site has to: the transports bound dialing, TLS and the wait for headers,
and after the headers only this bound and the caller's context end a body that
stops moving. An answer that does not reach the client is never read: the
retrying transport closes an answer it retries unread, and `closingRedirects`
closes a redirect's body unread before net/http, which would read up to 2 KB of
it, follows the redirect, and puts an empty body in its place, so that body is
closed once and what net/http reads and closes is the empty one; the client's
own redirect policy still decides, and without one net/http's limit of 10
holds (`TestRetryTransport_ARetriedAnswerIsClosedUnread`,
`TestGetItem_ARetriedAnswerWhoseBodyStallsIsNotWaitedOn`,
`TestGetItem_ARedirectWhoseBodyStallsIsFollowed`,
`TestGetItem_TheBodyOfARedirectIsClosedOnceAndNeverRead`,
`TestGetItem_RedirectsStopAtTheLimitOfNetHTTP`). Each
read starts a timer of the stall window (`defaultStallTimeout`, 2 minutes, the
transfer window) and its return stops it; a timer that fires closes the body,
which ends the pending read, and the read fails with the retryable
`ErrResponseStalled`, explained as a transfer stall is (`explainStall`: the
closed body's error kept as text only). Only time spent waiting in a read
counts, so a body that keeps moving, however slowly, is never failed; an error
body that stalls returns its answer's `*GraphError` with the placeholder
message, and a stalled drain fails like a stalled decode
(`TestGetItem_AStalledBodyFailsWithinTheStallWindow`,
`TestGetItem_AStalledBodyFailsWithinTheStallWindowOverHTTP2`,
`TestGetItem_ABodyThatKeepsMovingIsNotFailed`,
`TestGetItem_AStalledErrorBodyReturnsItsAnswer`,
`TestDeleteItem_AStalledDrainFailsWithinTheStallWindow`). No request of the
client legitimately stays silent longer: none is a long poll, and the
notification channel is a websocket that sync dials itself. A request on an
HTTP/2 connection the health check closed for an unanswered ping, and a body
being read on it, fail with an error net/http gives no type; its text
(`http2ConnectionLostText`) is read in one place, `markLostConnection`, which
both the round trip and the body read pass through and which gives either
failure `ErrConnectionLost` (`TestGetItem_AConnectionLostBeforeTheAnswerIsTyped`,
`TestGetItem_AConnectionLostDuringTheBodyIsTyped`: both produce it from the
real HTTP/2 client, so a Go release that rewords the text fails them). Sync
reads `ErrResponseStalled` and `ErrConnectionLost` as a request OneDrive never
answered.
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

**RetryAfter Header** — `RetryAfter time.Duration` field on `GraphError`, filled for 429 and 503 responses on every error path (Graph requests, pre-authenticated requests, raw uploads) by `retry.ResponseRetryAfter`, the one `Retry-After` reader (see [retry.md](retry.md), "Reading `Retry-After`"): delay-seconds or an HTTP-date measured against the response's `Date`, saturating rather than wrapping, never capped (`TestTerminalError_RetryAfter`). Implements: R-6.8.6 [verified]

## Transport-Layer Retry

Implements: R-6.8.8 [verified]

Generic retry has been extracted from the graph client into `retry.RetryTransport`, an `http.RoundTripper` wrapper. The graph client no longer has generic retry loops (`doRetry`/`doPreAuthRetry` deleted), throttle state, or transport-layer Retry-After coordination. The transport only replays methods that are safe at that generic boundary (`GET`, `HEAD`, `OPTIONS`, `TRACE`, `PUT`, `DELETE`); unsafe `POST` and `PATCH` calls return their first transient response to the graph operation that owns the mutation semantics. The one exception is an operation that marks its request `retry.WithReplaySafe`: `CreateFile` and `CreateFolder`, whose `conflictBehavior=fail` makes a replay of a committed create answer 409 without creating anything (R-6.8.21, `TestCreate_TransientFailureIsResentByARetryingTransport`). Before that, one 503 on a create failed `put` outright (e2e `TestE2E_Sync_UploadOnlyDefersRemoteOnlyChanges`). The only client-local retries are documented Graph API quirk normalizations: transient 403 on `/me/drives`, transient 404 on root-child listing, transient 404 on item-by-ID download metadata fetch before a pre-authenticated download begins, transient 404 or 502/503/504 on the immediate post-simple-upload `UpdateFileSystemInfo` PATCH, transient 404 on the create of a new file under a freshly created parent, and the documented simple-upload fallback path. All other retry, backoff, Retry-After parsing, and shared 429 coordination live below `graph` in the retry/HTTP-profile layer.

`NewClient` accepts an `*http.Client` and returns `(*Client, error)` after validating the base URL, the token source and the HTTP client; a nil HTTP client is an error, not `http.DefaultClient`, whose missing deadlines would undo the transport profiles (R-6.8.27). `MustNewClient` is reserved for static/test setup where panic-on-bad-construction is intentional. `driveops.SessionRuntime` is the normal production runtime owner for those clients, and it composes the stateless `internal/graphtransport` builders into the concrete profiles the app needs:

- interactive bootstrap/CLI metadata clients compose `RetryTransport{Policy: retry.TransportPolicy()}`, one per sign-in file, so a `Retry-After` delays only the throttled sign-in's requests
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
(`TestPollCopyStatus_NonSuccessStatusReturnsGraphError`). The poll goes
through `doPreAuth` like every other pre-authenticated request, so a retry
names it in the transport's log as `preauth:copy status`, never by its URL,
and it carries the User-Agent (R-6.8.17, R-6.8.18,
`TestPollCopyStatus_RetryLogNeverNamesTheMonitorURL`).

Pre-authenticated requests (upload chunks, downloads) go through `httpClient.Do()` directly. The `doPreAuth` helper sets `req.GetBody` using the `makeReq` factory so `RetryTransport` can rewind request bodies between attempts (e.g., `io.SectionReader` for upload chunks). It also attaches a request-scoped redacted log target such as `preauth:upload chunk`, so transport retry logs never emit the pre-authenticated URL itself. Transport failures come back from `http.Client.Do` as `*url.Error`, whose text embeds the full request URL; `dispatchRequest` scrubs it to scheme and host (`redactURLError`) before the error is wrapped, so surfaced error text and logs never carry the credential either. Implements: R-6.8.17 [verified]

## Refresh Across Processes

The sign-in file is the one authority between processes that use the same
account (a watch and a one-shot command, two commands): each holds its own
token in memory, and the sign-in service replaces the refresh token on a
refresh and refuses the replaced one, so a process that redeemed its stale
copy used to be told "sign-in required" (R-6.8.30). Every refresh
(`refreshShared`, `auth_refresh.go`) therefore runs under the file's lock,
`tokenfile.AcquireLock` (`<token>.json.lock` beside it, built on the shared
`filelock` package; token discovery takes only `.json` names):

1. Take the lock, waiting no longer than `signInLockWait` (90 s) or what is
   left of the starting caller's deadline, whichever is shorter. The bound is
   longer than the longest sign-in request (the auth client's 60 s
   `Timeout`; the two constants name each other), so a holder stuck in its
   request releases before a waiter gives up. A wait that ends fails with
   `tokenfile.ErrLockWaitEnded`, "another onedrive-go process is refreshing
   the sign-in", never "sign-in required". A filesystem that cannot lock
   (`tokenfile.ErrLockUnsupported`) logs one Warn per token source and the
   refresh goes on without it.
2. Read the file. No file means the account was signed out: "sign-in
   required" (`ErrNotLoggedIn`), no request, nothing written, and a pending
   failed save is dropped, so a running watch never undoes a sign-out.
3. A file that holds neither the refresh token in memory nor the start of
   this source's unsaved chain (the saver's lineage rule) was written by
   another process. Its access token is used, with no request, while it is
   valid; for a refresh a 401 forced, whenever it is not the rejected one.
   The one Info line is "found a newer sign-in in the file and used it".
   Otherwise its refresh token is the one redeemed. A file that is this
   source's, or the start of its unsaved chain, has the refresh token in
   memory redeemed, so a sign-in whose saves keep failing stays signed in
   (`TestRefresh_PersistentSaveFailureStaysSignedIn`).
4. Redeem it and save the answer under the same lock (`tokenSaver`, which
   keeps a failed save pending and retries it under the lock at the next
   token use, only while the file exists and still holds the start of the
   chain).
5. A refresh token the service refuses (`invalid_grant`) makes the file be
   read once more; a different refresh token there (a writer that took no
   lock, or a filesystem without locks) is redeemed, once; otherwise the
   refusal stands.

Login installs its sign-in through `tokenfile.Install` (an atomic write with
the directory synced, then the pending file removed) and undoes it on a
failed enrollment through `tokenfile.WithLock`; logout deletes through
`graph.Logout` under the same lock, tells a running watch (whose reload
drops its token sources), and a purge also removes the lock file. That the
sign-in service refuses a replaced refresh token is the behaviour of the
local test endpoint; it is not tested against the live service, and the
client does not depend on the answer: with a service that accepts it, the
lock only saves a request
(`TestRefresh_TwoSourcesOneFile_SecondAdoptsFirstsToken`,
`TestRefresh_RotatedTokenOnDiskIsUsedInsteadOfStaleMemory`,
`TestRefresh_InvalidGrantRereadsAndRetriesOnce`,
`TestRefresh_MissingFileIsSignInRequiredAndFileStaysAbsent`,
`TestForceRefresh_AdoptsNewerTokenFromDisk`,
`TestRefresh_LockWaitIsBounded`,
`TestRefresh_LockWaitEndsWithTheCallersDeadline`,
`TestAcquireLock_ExcludesAnotherHandleUntilReleased`,
`TestAcquireLock_ExcludesAnotherProcess`,
`TestDiscoverCIDFiles_SkipsTheSignInLockFile`,
`TestLogin_InstallsTokenUnderLock`, `TestLogout_NotifiesDaemon`). A failed
enrollment's undo puts back the sign-in the install replaced, read under the
install's lock, and waits for the lock even when the login was canceled
(`TestLogin_UndoRestoresTheSignInTheInstallReplaced`,
`TestInstall_UndoAfterTheLoginEndedWaitsForTheLock`).

## 401 Token Refresh

Implements: R-6.8.14 [verified], R-6.6.34 [verified]

Transparent token refresh on 401 inside `doOnce()`, independent of retry transport. On 401: refresh token → retry once. If still 401 → return `ErrUnauthorized` (fatal). Auth refresh is lifecycle management, not transient retry — it works regardless of whether the HTTP client has a `RetryTransport`.

The retry sends exactly the bytes of the first request. `do`, `doRequest`,
`doRequestVia`, `doOnce`, `retryUnauthorized` and `buildAuthorizedRequest`
take the JSON body as `[]byte` (nil for none), and every attempt is built from those bytes through a
fresh `bytes.Reader`, which also gives the request its exact `Content-Length`
and the `GetBody` the transport's own replay uses (R-6.8.19,
`spec/design/retry.md`). The type is the guard: a stream that a second
request could not resend cannot be passed at all. When the helpers took an
`io.Reader`, the 401 retry rewound the caller's reader with `Seek(0)` and
failed loudly on one that could not seek; a reader that stood past bytes
already read was rewound to the start of its buffer, and the retry sent bytes
the first request had not (`TestDo_401RetrySendsTheBytesOfTheFirstRequest`,
`TestDoOnce_401_RefreshSucceedsWithBodyAndQueryPreserved`,
`TestDoOnce_401_RefreshSucceeds`). Every authenticated caller sends a small
marshaled JSON body or none; content goes through `doRawUpload`, whose body
is a fresh `io.SectionReader` over the caller's `io.ReaderAt` on every
attempt, or through pre-authenticated URLs whose request builder runs again
for each attempt. A request with a body declares `Content-Type:
application/json`; `RestoreItem`, a body-less POST, passes the header itself,
as it always has (`TestDo_ContentTypeForBody`, `TestRestoreItem_Success`).

A failure before a request leaves carries `errRequestNotSent`: the token
source failed (a refresh the request's context cut short included, with that
context's cause), the context had already ended (net/http would not send it
either; `buildAuthorizedRequest` reports it as not sent rather than leaving
it to read as a dispatch failure), or the request could not be built. After a 401 the same holds for the retry: a
failed token refresh, a failed forced refresh, a 401 body that would not
close, or a retry that cannot be built leaves the refused request as the only
one sent. `doPreAuth` and `doRawUpload` mark a context that had ended and a
request that could not be built (or, for `doRawUpload`, whose token could not
be had) the same way, and the quirk retry marks a backoff that its context
ended. `dispatchRequestWith` marks a request the retrying transport returned
unsent because the throttle wait outlasts its deadline
(`*retry.ThrottledError`), and adds `ErrThrottled`: it is the earlier 429's
answer. The mark is what keeps `ErrCreateOutcomeUnknown` and
`FinalFragmentError` off failures that cannot have changed anything
(`TestDo_ContextCancellation`, `TestDo_TokenError`,
`TestDo_ATokenRefreshCutByTheContextIsNotSent`,
`TestDo_AThrottleWaitPastTheDeadlineIsNotSent`,
`TestSimpleUploadToItem_TokenError`).

Ordinary Graph `401` is intentionally **not** a sync trial or backoff signal.
The graph boundary gets exactly one lifecycle retry: refresh the saved login
and retry the request once. If that still fails, callers receive
`ErrUnauthorized`. Sync treats that as a fatal catalog-owned account-auth
condition for the current pass or watch session, and CLI surfaces present it as
`Authentication required`.

The refresh is forced when needed. The token source refreshes only an expired
token, so a token the service revoked early came back unchanged and the second
attempt resent it. `tokenBridge.ForceRefresh(ctx)` (the optional
`forceRefresher` capability) redeems the refresh token now, under the
request's context like any refresh; concurrent 401s join the refresh under
way and share one token-endpoint call per 30 s
(`TestDoOnce_401_ForcesRefreshWhenCachedTokenIsUnchanged`,
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
- The token source (`tokenBridge`) owns the cached token and at most one refresh goroutine at a time. The goroutine is started by the first caller that needs a refresh, runs detached from every caller's cancellation, and ends with its one token-endpoint request, which the sign-in client's `Timeout` bounds; its boundary hands a panic to `panicguard.Recovered` and gives the error to its waiters. `Client` consumes the source synchronously and starts no goroutine of its own.
- The sole raw `http.Client.Do` production boundary is `client_preauth.go`, where validated pre-authenticated URLs are dispatched.
- `driveops.SessionRuntime` owns the long-lived HTTP clients, the auth client its default token sources refresh through, one token source per sign-in file, and any caller-scoped shared throttle gates; it lists the save failures of the sources it holds (`TokenSaveFailures`, R-6.8.26).
- Each token source owns the one writer of its sign-in file (`tokenSaver`): its mutex orders the writes, so it is held across a write, never across a refresh request. It has no background goroutines and no package-level mutable state; the runtime instance is the owner.
- Account-level calls made before or without a drive session (identity probes, drive and shared-item discovery, SharePoint search, shared-drive naming, share-URL resolution) get their client from `SessionRuntime.BootstrapClient(tokenPath)`: that sign-in file's own bootstrap metadata client (`BootstrapMeta(tokenPath)`, so a throttle deadline is the throttled sign-in's), the runtime's base URL and the runtime's token source for that file, the one the drive sessions use. The CLI builds no token source of its own; a source scan in `internal/cli` keeps it so (R-6.8.27, `TestCLIBootstrapClientsUseTheSessionRuntime`). The one direct construction is login, which holds the source the sign-in just produced. No session constructor takes a context: a source keeps none and refreshes under each request's own, so one client serves requests with any deadline.

## Design Constraints

- `graph/` exposes concrete types, not interfaces (`graph.Client`, `graph.Item`)
- `graph/` does NOT import `config/` — callers pass token paths directly
- `tokenfile/` imports no internal package but `fsroot` (stdlib, oauth2 and the rooted managed-state capability) and `filelock` (its sign-in lock), and the `tokenfile-only-fsroot-and-filelock` depguard rule holds it to that
- Pre-authenticated URLs (`@microsoft.graph.downloadUrl`, upload session URLs) bypass the Graph API — use `httpClient.Do(req)` directly, no base URL prefix, no auth headers. Never log these URLs.
- The authenticated-success hook is only a proof signal. It must remain optional, per-client, and side-effect free from the graph package's perspective. Graph owns when the hook fires; callers own any resulting state repair.
- `graphtransport` is the single home for Graph-facing HTTP transport profile construction. Metadata and transfer clients use transport-level deadlines with `Timeout = 0`; no Graph-facing client uses `http.Client.Timeout`. The auth client (`AuthClient`) is not Graph-facing: it talks to the identity platform, whose answers are small, so it sets a 60 s `http.Client.Timeout` on top of a 30 s response-header deadline (R-6.8.27).
- Interactive metadata clients may share 429 `Retry-After` coordination through an injected `retry.ThrottleGate`. The coordination scope is the narrowest CLI-known remote boundary: configured drive-root mounts use `(account email, driveID)`, configured mount roots use `(account email, driveID, remoteRootItemID)`, and direct shared-item commands use `(account email, remoteDriveID, remoteItemID)`. Broader account-wide or tenant-wide coordination is intentionally not inferred from ordinary file traffic.
- `driveops.SessionRuntime` selects bootstrap, target-scoped interactive, or sync profiles directly. The CLI and control plane no longer build one cache owner merely to inject it into another.
- Upload URLs are sensitive credentials. The `UploadURL` type implements `slog.LogValuer` with redaction, matching the `DownloadURL` pattern.
- Search API calls URL-escape query parameters to prevent special characters from breaking URL construction.
- `internal/tokenfile` is intentionally pure OAuth token I/O. It persists only the wrapped `oauth2.Token`, rejects unknown JSON fields, and never writes repo-specific metadata alongside the token.
- Token file reads and writes use `internal/fsroot` after config-driven token resolution, so open/read/temp-file creation/chmod/fsync/rename all stay inside one managed-state root capability: every one of them resolves through an open `os.Root` descriptor, so a symlinked directory below the token folder cannot redirect a write, a rename or a removal outside it (R-6.4.10; `TestRoot_AtomicWrite_RefusesSymlinkedParent`, `TestRoot_Rename_RefusesSymlinkedParent`, `TestRoot_Remove_RefusesSymlinkedParent`, `TestRoot_CreateTemp_RefusesSymlinkedDir`).
- `client_preauth.go` is the sole raw `http.Client.Do` production boundary. Graph base URLs and pre-auth URLs are validated before a request reaches that call; the remaining inline `gosec` suppression there is intentional because the linter cannot prove the validation flow. The validators are one `urlValidators` set built by `newURLValidators`: `uploadURL`, `downloadURL`, `contentRedirect` (all the content host allow-list), `copyMonitor` and `socketIO`. A loopback base URL — admitted by `validateGraphBaseURL` only for test fixtures, and not settable by users — wraps each in `loopbackTolerant`, so a fixture's loopback pre-auth URLs are accepted while every other host still faces the production list (`TestNewClient_LoopbackBaseURLAcceptsLoopbackPreAuthURLs`). Production clients never get the wrapper.
- Graph/API quirk handling requires either captured payload evidence in CI/tests/logs or a reproducible documented observation with enough detail to classify safely. One-off incidents without recoverable payload evidence stay documented as reference notes only and do not become permanent runtime normalization rules.
- Broader throttle coordination beyond the proved remote target is deferred until ordinary file traffic provides reliable bucket identity or equally strong evidence for widening. Today the client intentionally blocks only the narrowest known drive/shared boundary. [planned]
- Upload, download and async-copy pre-auth URL validation: verify HTTPS scheme and trusted Microsoft hosts on upload session, `@microsoft.graph.downloadUrl` and copy monitor URLs before use. Both copy monitor and upload-session validation explicitly allow Personal-account URLs on `microsoftpersonalcontent.com` after live `e2e_full` coverage captured upload-session URLs on that host family. [verified]
- Audit all `slog.*` calls for potential secret leakage (tokens, pre-auth URLs). [verified]
- Audit all error message strings for embedded secrets — `GraphError.Message` and `RawBody` are redacted before exposure. [verified]
- Test that captures log output and verifies no tokens or pre-auth URLs appear. [verified]
- Authenticated request helpers are package-internal (`do`, `doGetWithHeaders`, and `doRequest`) and take the body as bytes. External callers use higher-level graph operations instead of raw request dispatch. [verified]
- Shared-item discovery uses only `GET /me/drive/root/search(q='*')`. Search
  results may omit owner identity or omit some shares entirely, so callers
  enrich actionable hits through `GET /drives/{driveId}/items/{itemId}` and
  surface caller-level degraded status when live search itself fails. [verified]
- Monitor `search(q='*')` reliability on business accounts for shared item discovery. Current coverage is characterization and unit/spec validation only; no business live fixture exists yet. [planned]
- `PermanentDeleteItem` 405→`DeleteItem` fallback for Personal accounts is a workaround. Remove when MS adds Personal support.

## Struct Tag Policy

Repo-wide JSON/TOML linting treats CLI and persisted payloads as snake_case by default. `internal/graph` is the deliberate exception: Graph wire structs keep upstream camelCase tags, and annotation-backed fields such as `@odata.nextLink` and `@microsoft.graph.downloadUrl` are exempted in lint configuration rather than suppressed inline.
