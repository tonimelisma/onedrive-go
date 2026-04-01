# Graph Client

GOVERNS: internal/graph/auth.go, internal/graph/client.go, internal/graph/delta.go, internal/graph/download.go, internal/graph/drives.go, internal/graph/errors.go, internal/graph/items.go, internal/graph/normalize.go, internal/graph/types.go, internal/graph/upload.go, internal/tokenfile/tokenfile.go

Implements: R-3.1 [verified], R-6.7 [implemented], R-6.8 [verified], R-1.1 [verified], R-1.4 [verified], R-1.5 [verified], R-1.6 [verified], R-1.7 [verified], R-1.8 [verified], R-6.7.4 [verified], R-6.7.8 [verified], R-6.7.9 [verified], R-6.7.10 [verified], R-6.7.11 [planned], R-6.7.12 [verified], R-6.7.13 [verified], R-6.7.17 [implemented], R-6.7.18 [planned], R-6.7.22 [planned], R-6.7.23 [planned], R-6.8.4 [planned], R-6.8.6 [verified], R-6.8.8 [verified], R-6.8.14 [verified]

## Overview

All Microsoft Graph API communication flows through `graph.Client`. The client is a pure API mapper plus a narrow Graph-quirk normalizer: authentication, quirk-specific retries, and error construction. Generic transient retry, throttle state, and backoff coordination stay in the transport layer. Callers receive clean, consistent data via `graph.Item` and never deal with API inconsistencies.

Retry lives in the transport layer via `retry.RetryTransport` (an `http.RoundTripper`). CLI callers wrap their HTTP client with `RetryTransport{Policy: Transport}` (5 retries). Sync callers use a raw HTTP client — failed requests return immediately for engine-level classification.

## Authentication (`auth.go`)

Two OAuth2 flows:
- **Device code flow** (default): user enters code on microsoft.com
- **PKCE authorization code flow** (`--browser`): localhost callback with code verifier

Token refresh is automatic. Tokens stored as JSON files via `tokenfile` package (strict JSON — rejects unknown fields). `tokenfile.Load` returns `ErrNotFound` sentinel (never nil,nil).

## Quirk Normalization (`normalize.go`)

All API quirks handled at the graph boundary — downstream code never sees them:
- DriveId casing normalization (lowercase)
- DriveId truncation fix (zero-pad Personal IDs to 16 chars)
- Delta deletion reordering (deletions before creations within each page)
- Missing field recovery (name, size for deleted items)
- Timestamp validation
- `parentReference.path` is never returned in delta — items tracked by ID
- Exact transient 403 retry on `GET /me/drives` when the Graph code chain contains `accessDenied`
- Exact transient 404 retry on `GET /drives/{driveID}/items/root/children` when the Graph code chain contains `itemNotFound`

## Delta Queries (`delta.go`)

Paginated delta with normalization pipeline. Returns `[]Item` (clean, normalized). HTTP 410 → `ErrGone` sentinel, caller triggers full re-enumeration. Supports folder-scoped delta for shared folders.

The client owns its safety guards and Graph-specific request metadata:

- `maxDeltaPages`: upper bound for full delta enumeration
- `maxRecursionDepth`: upper bound for recursive child listing
- `driveDiscoveryPolicy`: transient 403 retry policy for `/me/drives`
- `rootChildrenPolicy`: transient 404 retry policy for `root/children`
- `deltaPreferHeader`: prebuilt alias-ID delta header

These are instance fields on `graph.Client`, not package globals. Tests in package `graph` override them per client instance instead of mutating shared process state.

## Item Operations (`items.go`)

GetItem, ListChildren, CreateFolder, MoveItem, CopyItem, DeleteItem. All operations use `graph.Item` — the clean type after normalization.

## Transfers

- `download.go`: streaming download with content URL
- `upload.go`: simple PUT (≤4 MiB) and resumable upload sessions (>4 MiB, 320 KiB-aligned chunks)

## Error Handling (`errors.go`)

Sentinel errors: `ErrGone` (410), `ErrNotFound` (404), `ErrThrottled` (429), `ErrConflict` (409). Error response bodies are read with a 64 KiB cap (`io.LimitReader`) to prevent unbounded memory allocation from malformed responses. HTTP 423 (Locked) from SharePoint co-authoring is classified as skip, not retryable — locks persist for hours; watch mode retries on the next safety scan.

`GraphError` preserves Graph's structured error metadata: `Code`, `InnerCodes`, capped `RawBody`, and helper methods `MostSpecificCode()` / `HasCode()`. Quirk retries key on the code chain first. One-off incident signatures without recoverable payload evidence stay in the reference layer and are not special-cased in runtime classification.

**RetryAfter Header** — `RetryAfter time.Duration` field on `GraphError`. Parsed from `Retry-After` header for 429 and 503 responses. Implements: R-6.8.6 [implemented]

## Transport-Layer Retry

Implements: R-6.8.8 [verified]

Generic retry has been extracted from the graph client into `retry.RetryTransport`, an `http.RoundTripper` wrapper. The graph client no longer has generic retry loops (`doRetry`/`doPreAuthRetry` deleted), throttle state, or transport-layer Retry-After coordination. The only client-local retries are documented Graph API quirk normalizations: transient 403 on `/me/drives` and transient 404 on root-child listing. All other retry, backoff, Retry-After parsing, and 429 throttle coordination live in the transport layer.

`NewClient` accepts an `*http.Client` — the caller decides whether that client's transport includes retry. CLI callers wrap with `RetryTransport`. Sync callers pass a raw client.

Pre-authenticated requests (upload chunks, downloads) go through `httpClient.Do()` directly. The `doPreAuth` helper sets `req.GetBody` using the `makeReq` factory so `RetryTransport` can rewind request bodies between attempts (e.g., `io.SectionReader` for upload chunks). It also attaches a request-scoped redacted log target such as `preauth:upload chunk`, so transport retry logs never emit the pre-authenticated URL itself.

## 401 Token Refresh

Implements: R-6.8.14 [implemented]

Transparent token refresh on 401 inside `doOnce()`, independent of retry transport. On 401: refresh token → retry once. If still 401 → return `ErrUnauthorized` (fatal). Auth refresh is lifecycle management, not transient retry — it works regardless of whether the HTTP client has a `RetryTransport`.

## Design Constraints

- `graph/` exposes concrete types, not interfaces (`graph.Client`, `graph.Item`)
- `graph/` does NOT import `config/` — callers pass token paths directly
- `tokenfile/` is a leaf package (stdlib + oauth2 only)
- Token refresh uses a forked `golang.org/x/oauth2` (`github.com/tonimelisma/oauth2`, branch `on-token-change`) with `Config.OnTokenChange` callback for persistence. Tracks upstream proposal `golang/go#77502`.
- Pre-authenticated URLs (`@microsoft.graph.downloadUrl`, upload session URLs) bypass the Graph API — use `httpClient.Do(req)` directly, no base URL prefix, no auth headers. Never log these URLs.
- Two HTTP clients: `defaultHTTPClient()` (30-second timeout for metadata) and `transferHTTPClient()` (no client-level timeout for large file transfers). Transfer clients use `transferTransport()` with `ResponseHeaderTimeout` and TCP keepalives to detect stalled/dead connections without bounding total transfer time.
- Upload URLs are sensitive credentials. The `UploadURL` type implements `slog.LogValuer` with redaction, matching the `DownloadURL` pattern.
- Search API calls URL-escape query parameters to prevent special characters from breaking URL construction.
- Token metadata validation is enforced on both write and read paths. Required fields must be present in non-nil metadata. `tokenfile.ValidateMeta()` validates before save, `LoadAndValidate()` validates on load.
- Token file reads use root-based trusted-path helpers after config-driven token resolution.
- `dispatchRequest` is the sole raw `http.Client.Do` boundary. Graph base URLs and pre-auth URLs are validated before a request reaches that call; the remaining inline `gosec` suppression there is intentional because the linter cannot prove the validation flow.
- Graph/API quirk handling requires either captured payload evidence in CI/tests/logs or a reproducible documented observation with enough detail to classify safely. One-off incidents without recoverable payload evidence stay documented as reference notes only and do not become permanent runtime normalization rules.
- Per-tenant rate limit coordination: multiple drives under the same tenant share Graph API rate limits. A shared rate limiter per-tenant prevents aggregate throttling. [planned]
- Upload and async-copy pre-auth URL validation: verify HTTPS scheme and trusted Microsoft hosts on upload session and copy monitor URLs before use. Copy monitor validation explicitly allows Personal-account monitor URLs on `microsoftpersonalcontent.com`; upload-session validation remains narrower until separate evidence appears. [verified]
- Audit all `slog.*` calls for potential secret leakage (tokens, pre-auth URLs). [verified]
- Audit all error message strings for embedded secrets — `GraphError.Message` includes API error body. [planned]
- Test that captures log output and verifies no tokens or pre-auth URLs appear. [verified]
- Evaluate unexporting `graph.Client.Do`/`DoWithHeaders` if unused outside the package. [planned]
- Monitor `search(q='*')` reliability on business accounts for shared item discovery. [planned]
- `PermanentDeleteItem` 405→`DeleteItem` fallback for Personal accounts is a workaround. Remove when MS adds Personal support.

## Struct Tag Policy

Repo-wide JSON/TOML linting treats CLI and persisted payloads as snake_case by default. `internal/graph` is the deliberate exception: Graph wire structs keep upstream camelCase tags, and annotation-backed fields such as `@odata.nextLink` and `@microsoft.graph.downloadUrl` are exempted in lint configuration rather than suppressed inline.
