# Graph Client

GOVERNS: internal/graph/auth.go, internal/graph/client.go, internal/graph/delta.go, internal/graph/download.go, internal/graph/drives.go, internal/graph/errors.go, internal/graph/items.go, internal/graph/normalize.go, internal/graph/types.go, internal/graph/upload.go, internal/tokenfile/tokenfile.go

Implements: R-3.1 [implemented], R-6.7 [implemented], R-6.8 [implemented], R-1.1 [implemented], R-1.4 [implemented], R-1.5 [implemented], R-1.6 [implemented], R-1.7 [implemented], R-1.8 [implemented]

## Overview

All Microsoft Graph API communication flows through `graph.Client`. The client handles authentication, retry, rate limiting, and API quirk normalization internally. Callers receive clean, consistent data via `graph.Item` and never deal with API inconsistencies.

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

## Delta Queries (`delta.go`)

Paginated delta with normalization pipeline. Returns `[]Item` (clean, normalized). HTTP 410 → `ErrGone` sentinel, caller triggers full re-enumeration. Supports folder-scoped delta for shared folders.

## Item Operations (`items.go`)

GetItem, ListChildren, CreateFolder, MoveItem, CopyItem, DeleteItem. All operations use `graph.Item` — the clean type after normalization.

## Transfers

- `download.go`: streaming download with content URL
- `upload.go`: simple PUT (≤4 MiB) and resumable upload sessions (>4 MiB, 320 KiB-aligned chunks)

## Error Handling (`errors.go`)

Sentinel errors: `ErrGone` (410), `ErrNotFound` (404), `ErrThrottled` (429), `ErrConflict` (409). The client respects `Retry-After` headers and uses exponential backoff for transient failures. Error response bodies are read with a 64 KiB cap (`io.LimitReader`) to prevent unbounded memory allocation from malformed responses. HTTP 423 (Locked) from SharePoint co-authoring is classified as skip (`errClassSkip`), not retryable — locks persist for hours; watch mode retries on the next safety scan.

## Design Constraints

- `graph/` exposes concrete types, not interfaces (`graph.Client`, `graph.Item`)
- `graph/` does NOT import `config/` — callers pass token paths directly
- `tokenfile/` is a leaf package (stdlib + oauth2 only)
- Token refresh uses a forked `golang.org/x/oauth2` (`github.com/tonimelisma/oauth2`, branch `on-token-change`) with `Config.OnTokenChange` callback for persistence. Tracks upstream proposal `golang/go#77502`.
- Pre-authenticated URLs (`@microsoft.graph.downloadUrl`, upload session URLs) bypass the Graph API — use `httpClient.Do(req)` directly, no base URL prefix, no auth headers. Never log these URLs.
- Two HTTP clients: `defaultHTTPClient()` (30-second timeout for metadata) and `transferHTTPClient()` (no timeout for large file transfers, bounded only by context).
- Upload URLs are sensitive credentials. The `UploadURL` type implements `slog.LogValuer` with redaction, matching the `DownloadURL` pattern.
- Search API calls URL-escape query parameters to prevent special characters from breaking URL construction.
- Token metadata validation is enforced on both write and read paths. Required fields must be present in non-nil metadata. `tokenfile.ValidateMeta()` validates before save, `LoadAndValidate()` validates on load.
- Per-tenant rate limit coordination: multiple drives under the same tenant share Graph API rate limits. A shared rate limiter per-tenant prevents aggregate throttling. [planned]
- Upload URL validation: verify HTTPS scheme and Microsoft domain on `UploadSession.UploadURL` before use. [planned]
- Audit all `slog.*` calls for potential secret leakage (tokens, pre-auth URLs). [planned]
- Audit all error message strings for embedded secrets — `GraphError.Message` includes API error body. [planned]
- Test that captures log output and verifies no tokens or pre-auth URLs appear. [planned]
- Evaluate unexporting `graph.Client.Do`/`DoWithHeaders` if unused outside the package. [planned]
- Monitor `search(q='*')` reliability on business accounts for shared item discovery. [planned]
- The `PermanentDeleteItem` 405→`DeleteItem` fallback for Personal accounts is a workaround. Remove when MS adds Personal support. [planned]
