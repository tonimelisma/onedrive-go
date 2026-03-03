# Graph API Issues & Workarounds

Known Microsoft Graph API behavioral issues encountered in this project, the workarounds in production code and tests, and the evidence behind each.

---

## 1. Transient 404 on Valid Resources

**Observed**: Multiple CI runs — `GET /drives/{driveID}/items/root/children` returned HTTP 404 ("itemNotFound") on a drive that has existed for months. The token is valid, the drive ID is correct, and subsequent requests succeed.

**Evidence**:
- Run 22590221325 (2026-03-02T18:37): `ls /` returned 404 in 0.71s. Request ID `5583219e-c8bf-4899-94d6-aff7b4f1f554`. Re-run passed. The `no_config` variant of the same test passed in 0.55s on the same run — proving the drive was accessible, just not on the first request.
- Earlier observation: CI run #152 attempt 1 — same endpoint returned 404 after **13 seconds** of server-side processing. The 13-second response time (vs normal <1s) is the signature of a cross-datacenter lookup timeout.

**Cause**: Microsoft's Graph API load balancer routes each TCP connection to a backend node. If that node doesn't have the drive's data cached, it attempts an internal cross-datacenter lookup. When this lookup times out (13s variant) or the node's cache is cold (sub-1s variant), the backend returns 404 ("itemNotFound") instead of a 5xx error.

**This is NOT eventual consistency** — the resource has existed for months. It's a transient infrastructure failure misreported as a client error.

**Production workaround**: The sync executor (`internal/sync/executor.go`) classifies HTTP 404 as retryable in `classifyStatusCode`, so all sync actions (download, upload, mkdir, etc.) automatically retry transient 404s with exponential backoff. The lower-level Graph client (`internal/graph/client.go`) still treats 404 as non-retryable, which is correct for CLI file operations where a true 404 should fail fast. Delete actions handle 404 as success before reaching the retry logic, so this does not cause spurious delete retries.

**Test workaround**: None for `ls /` (root listing). The test calls `runCLIWithConfig(t, cfgPath, nil, "ls", "/")` which fails fast on error — no polling, no outer retry. A CI rerun resolves the issue. Adding polling would mask genuine regressions (e.g., broken drive ID resolution would silently retry for 30s then fail with a confusing timeout).

**Affected tests**: `TestE2E_RoundTrip/ls_root`, `TestE2E_JSONOutput/ls_json` — both call `ls /` without polling because root listing has no mutation dependency (the root always exists). The `TestE2E_ErrorCases/get_root_is_folder` test is also theoretically affected but expects an error, so a 404 would appear as a different error type rather than the expected "cannot download folder" message.

**Frequency**: Low — 1 occurrence in the last 100 CI runs (1%). Most `ls /` calls succeed in <1s.

---

## 2. Eventual Consistency After Mutations

**Observed**: After uploading a file via `PUT`, `GET` by path or `ls` of the parent folder can return 404 for several seconds. This is standard read-after-write eventual consistency — the write hits one replica, reads may hit a different replica that hasn't received the update yet.

**Scope**: Affects all operations that read recently-created or modified items: `ls`, `stat`, `get` by path after `put`, `mkdir`, or `rm`.

**Production workaround**: None needed — the sync engine uses delta queries (server pushes changes), not path-based polling.

**Test workaround**: The E2E test suite has polling helpers (`e2e/sync_e2e_test.go`):

```go
const pollTimeout = 30 * time.Second  // covers observed propagation delays

func pollBackoff(attempt int) time.Duration {
    // 500ms, 1s, 2s, 4s cap — exponential with ceiling
}
```

Two polling functions, each retrying a CLI command until success or timeout:
- `pollCLIWithConfigContains(t, cfgPath, env, expected, timeout, args...)` — retries until stdout contains expected string
- `pollCLIWithConfigSuccess(t, cfgPath, env, timeout, args...)` — retries until exit code 0

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

**Non-retryable client errors** (at Graph client level): 400, 401, 403, 404, 409. These are returned immediately. Note: the sync executor retries 404 at a higher level (see §1) because transient 404s are common on valid resources.

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

## 13. Transient 403 on `/me/drives` (Token Propagation Delay)

**Observed**: CI run #160 post-merge — `GET /me/drives` returned HTTP 403 ("accessDenied") three times in succession, exhausting the retry budget. The token was valid (the preceding `GET /me` returned 200 with user profile data). The failure only affected one subtest; a re-run passed immediately.

**Cause**: Microsoft's token propagation infrastructure has an eventual-consistency window. After a fresh OIDC token exchange (or token refresh), the `/me/drives` endpoint may reject the token with 403 before all Graph API backend nodes have received the updated authorization state. The `/me` endpoint (user profile) is more resilient — it operates on a different backend that receives token propagation earlier.

This is distinct from a genuine permissions issue (which would fail consistently). The signature is: `/me` succeeds → `/me/drives` returns 403 → retry with the same token eventually succeeds (or doesn't within the retry budget).

**Production workaround** (`internal/graph/drives.go`):

```go
const driveDiscoveryRetries = 3

func (c *Client) Drives(ctx context.Context) ([]Drive, error) {
    for attempt := range driveDiscoveryRetries {
        drives, err := c.drivesList(ctx)
        if err == nil {
            return drives, nil
        }
        // Only retry on 403 (transient accessDenied during token propagation).
        var ge *GraphError
        if !errors.As(err, &ge) || ge.StatusCode != http.StatusForbidden {
            return nil, err
        }
        // ... exponential backoff with jitter ...
    }
    return nil, lastErr
}
```

The `Drives()` function retries up to 3 times on 403 only, with exponential backoff (1s, 2s, 4s base with ±25% jitter). Non-403 errors fail immediately. Design decision DP-11 (`docs/design/decisions.md`) documents why this retry is domain-specific rather than added to the general retry set — other 403s (permission denied, retention policy) are permanent and should not be retried.

**Test coverage** (`internal/graph/drives_test.go`):
- `TestDrives_Transient403_Recovers` — simulates 403, 403, 200 (success on 3rd attempt)
- `TestDrives_Permanent403_ExhaustsRetries` — simulates 403 × 3 (exhausts budget, returns ErrForbidden)
- `TestDrives_NonForbidden_NoRetry` — simulates 401 (fails immediately, no retry)

**CI evidence**: Run #160 post-merge (run ID 22603739179). The `whoami --text` subcommand calls `client.Drives()` to list all accessible drives (for the human-readable output showing drive details, quota, and type). All 3 retry attempts returned 403. Re-run passed.

**Frequency**: Rare. First observed after dual-token CI setup where both E2E accounts' tokens are refreshed in the same job. The token propagation delay appears more likely when two accounts' tokens are refreshed in quick succession.

---

## 14. GitHub Actions Go Module Cache Corruption

**Observed**: Multiple CI runs (22590221325, 22571687313, 22562639738) show `tar: Cannot open: File exists` errors during the `actions/setup-go` cache restore step. Hundreds of files in the Go toolchain cache (`golang.org/toolchain@v0.0.1-go1.24.4.linux-amd64/`) fail to extract because they already exist on the runner.

**Cause**: The `actions/setup-go@v5` action caches `~/go/pkg/mod` (the Go module cache) between runs. When the GitHub Actions runner image already includes a pre-installed Go toolchain, and the cache was saved from a run that downloaded the same or overlapping toolchain files, the tar extraction step encounters file conflicts. The runner image has the files at the OS level, and the cache also has them — tar can't overwrite.

**Impact**: The tar extraction failure is reported as a **warning** by `actions/setup-go`, not an error. The step completes successfully because the Go toolchain is available from the OS-level installation. Tests still run with the correct Go version. However, GitHub Actions classifies the cache restore step as having annotations (`##[warning]Failed to restore`), which appear in the run summary.

**This is cosmetic** — it does not cause test failures. When these runs fail, it is always due to a separate issue (transient Graph API error, code bug, etc.). The tar warnings are a red herring.

**Workaround**: None needed. The warning is harmless. The Go cache key rotates naturally when `go.sum` changes (cache key includes a hash of `go.sum`). The issue is upstream in `actions/setup-go` and `actions/cache`.

**Evidence**: All affected runs show the pattern:
```
##[warning]Failed to restore: "/usr/bin/tar" failed with error: The process '/usr/bin/tar' failed with exit code 2
```
In annotations for lint, test, integration, and/or e2e jobs. The actual test pass/fail is independent of this warning.

---

## 15. HTTP 400 "ObjectHandle is Invalid" (Microsoft Outage Pattern)

**Observed**: Six consecutive CI runs between 2026-03-02T04:09 and 2026-03-02T07:19 UTC failed with HTTP 400 `invalidRequest` / `"ObjectHandle is Invalid"` on every Graph API call — `ls /`, `mkdir`, `put`, `stat`, and even integration tests. The error was not endpoint-specific; it affected all operations on both test accounts.

**Evidence** (all on 2026-03-02):
- Run 22561011885 (04:09 UTC): Integration tests — HTTP 400 ObjectHandle on all requests
- Run 22561231151 (04:19 UTC): E2E — `ls /`, `mkdir`, `put` all returned 400
- Run 22562639738 (05:24 UTC): E2E — same pattern
- Run 22564421183 (06:36 UTC): E2E — same pattern
- Run 22564882913 (06:54 UTC): E2E — same pattern
- Run 22565515068 (07:18 UTC): E2E — same pattern
- Run 22566127433 (07:39 UTC): **First success** — all tests passed

Total outage window: ~3.5 hours. All runs before and after the window passed.

**Cause**: Microsoft-side backend failure. "ObjectHandle is Invalid" is an internal Graph API error indicating that the backend storage node could not resolve the drive/item handle. The error code is `invalidRequest` (HTTP 400), which makes it non-retryable in our client. This is a server-side issue misclassified as a client error — the requests were valid.

**This is distinct from transient 404** (§1) — 404 affects a single request/endpoint, while ObjectHandle failures affect all endpoints simultaneously and persist for hours, indicating a backend-wide issue rather than a load-balancer routing anomaly.

**Production workaround**: None. HTTP 400 is correctly classified as non-retryable (`internal/graph/errors.go`). Adding 400 to the retry set would mask genuine client errors (malformed requests, invalid parameters). The correct response to an ObjectHandle outage is to wait for Microsoft to resolve it.

**Test workaround**: None. CI rerun after the outage window resolves the issue. The nightly cron job (`0 10 * * *`) will naturally retry the next day if the outage spans the scheduled run.

**Frequency**: Rare — 1 multi-hour incident in the project's history. No recurrence observed.

---

## 16. CI Failure Taxonomy (Last 100 Runs)

Analysis of the last 100 CI runs (as of 2026-03-03) shows 80% pass rate (80 success, 20 failure). Failures break down into distinct categories:

| Category | Count | Runs | Root Cause |
|----------|-------|------|------------|
| ObjectHandle outage (§15) | 6 | 22561011885–22565515068 | Microsoft backend failure |
| Azure OIDC login failure | 3 | 22562526251, 22564566115, 22564781140 | Azure identity platform transient |
| Token missing refresh_token | 1 | 22565640811 | CI credential download issue |
| Transient 404 on `ls /` (§1) | 1 | 22590221325 | Graph API load balancer cache miss |
| Transient 403 on `/me/drives` (§13) | 0 | (observed in run 22603739179) | Token propagation delay |
| E2E sync test (conflict history) | 1 | 22571687313 | Edit-delete conflict not recorded in history |
| Unit test regression | 2 | 22605768759, 22605754609 | `TestLoadAndResolve_MissingFile_NoDrives_Error` — error message changed |
| Drive ID not resolved | 2 | 22556801735 (+ others in tail) | Token file missing drive ID metadata |
| Lint/other | 1 | 22598950245 | Lint failure on branch |
| Unclassified (CI infra) | 3 | Various older runs | Azure login, credential pipeline |

**Key insight**: Only 2 of 20 failures (10%) are caused by code bugs. The remaining 90% are infrastructure issues (Microsoft outages, Azure OIDC transients, CI credential pipeline issues) or Graph API behavioral quirks. The E2E tests are fundamentally sound but operate against an unreliable external dependency.

**Actionable items (all addressed)**:
- The `TestLoadAndResolve_MissingFile_NoDrives_Error` failure (2 runs) — **fixed** in commit `39308c7`.
- The edit-delete conflict history test failure (1 run) — **root cause**: Graph API eventual consistency (remote delete not propagated before sync runs) + SQLite WAL cross-process visibility. **Fix**: (a) added `pollCLIWithConfigNotContains` guard between remote delete and sync in `TestE2E_Sync_EditDeleteConflict`; (b) added `PRAGMA wal_checkpoint(TRUNCATE)` to `BaselineManager.Close()`.
- The "drive ID not resolved" failure (2 runs) — **root cause**: token file missing mandatory metadata (`drive_id`). **Fix**: defense-in-depth validation via `tokenfile.ValidateMeta()` on both write (`Save()`) and read (`LoadAndValidate()`, `ReadTokenMeta()`) paths. Required keys: `drive_id`, `user_id`, `display_name`, `cached_at`.

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
