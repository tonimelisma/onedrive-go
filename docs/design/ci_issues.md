# Graph API Issues & Workarounds

Known Microsoft Graph API behavioral issues encountered in this project, the workarounds in production code and tests, and the evidence behind each.

---

## 1. Transient 404 on Valid Resources

**Observed**: CI run #152 attempt 1 — `GET /drives/{driveID}/items/root/children` returned HTTP 404 after **13 seconds** of server-side processing. The drive, token, and request were all valid. Every subsequent request to the same drive succeeded in <2s. The rerun passed in 0.69s.

**Cause**: Microsoft's Graph API load balancer routes each TCP connection to a backend node. If that node doesn't have the drive's data cached, it attempts an internal cross-datacenter lookup. When this lookup times out, the backend returns 404 ("itemNotFound") instead of a 5xx error. The 13-second response time (vs normal <1s) is the signature of this behavior.

**This is NOT eventual consistency** — the resource has existed for months. It's a transient infrastructure failure misreported as a client error.

**Production workaround**: None. Our retry logic (`internal/graph/client.go`) treats 404 as non-retryable, which is correct for the general case (a true 404 should not be retried). Adding 404 to the retry set would mask genuine "not found" errors and slow down normal error paths.

**Test workaround**: None for `ls /` (root listing). The test uses a direct `mode.run()` call and fails fast on error. A CI rerun resolves the issue. Adding polling would mask genuine regressions (e.g., broken drive ID resolution would silently retry for 30s then fail with a confusing timeout).

---

## 2. Eventual Consistency After Mutations

**Observed**: After uploading a file via `PUT`, `GET` by path or `ls` of the parent folder can return 404 for several seconds. This is standard read-after-write eventual consistency — the write hits one replica, reads may hit a different replica that hasn't received the update yet.

**Scope**: Affects all operations that read recently-created or modified items: `ls`, `stat`, `get` by path after `put`, `mkdir`, or `rm`.

**Production workaround**: None needed — the sync engine uses delta queries (server pushes changes), not path-based polling.

**Test workaround**: The E2E test suite has polling helpers (`e2e/e2e_test.go`):

```go
const pollTimeout = 30 * time.Second  // covers observed propagation delays

func pollBackoff(attempt int) time.Duration {
    // 500ms, 1s, 2s, 4s cap — exponential with ceiling
}
```

Four polling functions, each retrying a CLI command until success or timeout:
- `pollCLIContains(t, expected, timeout, args...)` — retries until stdout contains expected string
- `pollCLISuccess(t, timeout, args...)` — retries until exit code 0
- `pollCLIWithConfigContains(...)` — same with custom config file
- `pollCLIWithConfigSuccess(...)` — same with custom config file

**Where polling is used vs direct calls**:

| Operation | Polling? | Why |
|-----------|----------|-----|
| `whoami`, `status` | No | Auth endpoints, no mutation dependency |
| `ls /` (root) | No | Root always exists, no prior mutation |
| `ls /folder` after `mkdir` + `put` | Yes | File may not be visible on all replicas yet |
| `stat /file` after `put` | Yes | Same — read-after-write consistency lag |
| `stat /file` before `rm --permanent` | Yes | Must confirm file is visible before deleting |
| `mkdir`, `put`, `get`, `rm` | No | These are mutations, not reads — they either succeed or fail immediately |

---

## 3. Bogus Hashes on Deleted Items in Delta Responses

**Observed**: Delta responses include QuickXorHash, SHA1, or SHA256 hashes on items marked as deleted. These hashes are stale (from the item's last live state) or entirely bogus, and cause spurious hash mismatches during sync processing.

**Production workaround** (`internal/graph/normalize.go`, stage 3 of the normalization pipeline):

```go
// clearDeletedHashes zeroes out all hashes on deleted items.
// The Graph API sometimes returns stale or bogus hashes on deleted items
// in delta responses, which can cause spurious hash mismatches during
// sync processing.
```

All three hash fields (QuickXorHash, SHA1Hash, SHA256Hash) are cleared on any item where `IsDeleted == true`.

---

## 4. Duplicate Items in a Single Delta Batch

**Observed**: The same item can appear multiple times in a single delta response page, or across pages within one delta call. This happens when an item changes between the server generating different pages of the response.

**Production workaround** (`internal/graph/normalize.go`, stage 4):

```go
// deduplicateItems keeps only the last occurrence of each item ID.
// The Graph API can return the same item multiple times in a single
// delta batch when it changes between pages — only the final state matters.
```

A map keyed by item ID tracks indices; only the last occurrence survives.

---

## 5. Deletion/Creation Ordering Conflicts

**Observed**: When a file is renamed and a new file is created with the old name (at the same parent), the delta response may list the creation before the deletion. Processing in order causes a 409 Conflict ("item already exists").

**Production workaround** (`internal/graph/normalize.go`, stage 5):

```go
// reorderDeletions moves deleted items before non-deleted items
// within the same parent. This prevents "item already exists" errors
// when processing a rename-then-recreate at the same parent.
```

Within each parent ID group, deletions are sorted before creations.

---

## 6. URL-Encoded Item Names

**Observed**: Some Graph API responses return item names with URL encoding (e.g., `%20` for spaces, `%E6%97%A5%E6%9C%AC%E8%AA%9E.txt` for `日本語.txt`). This is inconsistent — most responses return plain UTF-8.

**Production workaround** (`internal/graph/normalize.go`, stage 1):

```go
// decodeURLEncodedNames decodes any %XX sequences in item names.
```

Applied unconditionally to all item names in delta responses.

---

## 7. HTTP 410 Gone — Delta Token Expired

**Observed**: Delta tokens expire after an unspecified period (observed: days to weeks). When a delta call uses an expired token, the server returns 410 Gone with a body indicating a resync is needed.

**Production workaround** (`internal/graph/items.go`): The delta function detects 410 responses and returns a sentinel error. The sync engine catches this and triggers a full re-enumeration (fresh delta with no token), which rebuilds the baseline from scratch.

---

## 8. HTTP 507 Insufficient Storage

**Observed**: OneDrive returns 507 when the user's storage quota is exceeded. This wraps `ErrServerError` at the sentinel level, which would incorrectly classify it as retryable.

**Production workaround** (`internal/sync/executor.go`):

```go
// GraphError carries a status code — classify by code for precision.
// This must come before sentinel checks because 507 (fatal) wraps
// ErrServerError (retryable), and the specific code wins.
var ge *graph.GraphError
if errors.As(err, &ge) {
    return classifyStatusCode(ge.StatusCode)
}
```

The executor checks `GraphError.StatusCode` before `errors.Is()` sentinel matching. 507 is classified as fatal — retrying a full disk is pointless.

---

## 9. Rate Limiting (429 Too Many Requests)

**Observed**: Graph API returns 429 with a `Retry-After` header (in seconds) when the caller exceeds per-user or per-app rate limits. This is more common during parallel E2E tests.

**Production workaround** (`internal/graph/client.go`):

```go
// For 429 (throttled), the Graph API's Retry-After header takes
// precedence over calculated backoff — ignoring it risks extended throttling.
```

The retry loop reads `Retry-After` from 429 responses and uses it verbatim. For non-429 retryable errors, exponential backoff (1s base, 2x factor, 60s cap, 25% jitter) is used. Max 5 retries.

**Retryable status codes**: 408, 429, 500, 502, 503, 504, 509 (SharePoint bandwidth limit).

**Non-retryable client errors**: 400, 401, 403, 404, 409. These are returned immediately.

---

## 10. Token Refresh Races Under Parallel Execution

**Observed**: When multiple CLI processes share the same token file and the access token expires, all processes attempt an OAuth2 refresh simultaneously. Each gets a new access token and a new refresh token. Last writer wins on disk.

**Risk assessment**: Negligible for CI. Microsoft grants a grace period where the old refresh token still works after a new one is issued. All processes succeed; the last-written refresh token is the one that persists. The `OnTokenChange` callback logs every refresh at INFO level: "token refreshed by oauth2 library" and "persisted refreshed token to disk".

**Production workaround** (`internal/driveops/session.go`):

```go
// SessionProvider caches TokenSources by token file path and creates
// Sessions on demand. Multiple drives sharing a token path share one
// TokenSource, preventing OAuth2 refresh token rotation races.
```

Within a single process, `SessionProvider` ensures one `TokenSource` per token file. Across processes (parallel E2E tests), last-write-wins is accepted.

**Test workaround**: Sync tests each get their own token copy via `writeSyncConfig()`. File-op tests share the TestMain-level token. With `-parallel 5`, at most 5 processes may refresh concurrently.

**CI workaround**: Integration and E2E jobs use **separate test accounts** (`ONEDRIVE_TEST_DRIVE_INTEGRATION` vs `ONEDRIVE_TEST_DRIVE_E2E`) so they never race on the same token.

---

## 11. Nightly Token Keepalive

**Observed**: Microsoft refresh tokens expire after 90 days of inactivity. If CI doesn't run for 90 days, tokens silently expire and all live-API tests fail with 401.

**CI workaround** (`.github/workflows/ci.yml`):

```yaml
schedule:
  - cron: '0 10 * * *'  # 10 AM UTC — keeps refresh tokens alive
```

The nightly run exercises the token (triggering a refresh if needed) and saves the rotated token back to Azure Key Vault.

**Recovery**: If tokens do expire, re-run `scripts/bootstrap-test-credentials.sh` (interactive, requires browser) followed by `scripts/migrate-test-data-to-ci.sh`.

---

## 12. Drive ID Casing Inconsistency

**Observed**: Graph API returns drive IDs with inconsistent casing across different endpoints (e.g., `F1DA660E69BDEC82` vs `f1da660e69bdec82`).

**Production workaround** (`internal/driveid/`): The `driveid.ID` type normalizes all drive IDs to lowercase and zero-pads to 16 characters on construction.

---

## Summary: The Normalization Pipeline

All delta responses pass through a 5-stage normalization pipeline (`internal/graph/normalize.go`) before the sync engine sees them:

```
Raw delta items
  → Stage 1: Decode URL-encoded names
  → Stage 2: Filter OneNote packages
  → Stage 3: Clear deleted item hashes
  → Stage 4: Deduplicate (keep last occurrence)
  → Stage 5: Reorder deletions before creations
Normalized items → sync engine
```

Each stage addresses a specific Graph API quirk observed in production or testing. The pipeline is tested end-to-end in `quirks_test.go`.
