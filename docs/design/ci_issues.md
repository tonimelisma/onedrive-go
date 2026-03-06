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
- The edit-delete conflict history test failure (1 run) — **root cause**: Graph API eventual consistency (remote delete not propagated before sync runs) + SQLite WAL cross-process visibility. **Fix**: (a) added `pollCLIWithConfigNotContains` guard between remote delete and sync in `TestE2E_Sync_EditDeleteConflict`; (b) added `PRAGMA wal_checkpoint(TRUNCATE)` to `SyncStore.Close()`.
- The "drive ID not resolved" failure (2 runs) — **root cause**: token file missing mandatory metadata (`drive_id`). **Fix**: defense-in-depth validation via `tokenfile.ValidateMeta()` on both write (`Save()`) and read (`LoadAndValidate()`, `ReadTokenMeta()`) paths. Required keys: `drive_id`, `user_id`, `display_name`, `cached_at`.

**Additional fixes (nightly CI 2026-03-04/05)**:
- `TestE2E_Status_NoDrives` — test asserted "drive add" but isolated HOME (no tokens) triggers "login" message. Fixed assertion.
- `TestE2E_Sync_TransferWorkersConfig` — `writeSyncConfigWithOptions` placed global keys (`transfer_workers`) inside `[drive]` section, rejected by `checkDriveUnknownKeys()`. Fixed TOML ordering.
- `TestE2E_Verify_JSON` / `TestE2E_CLI_Verify` — `VerifyReport.Mismatches` had `omitempty`, omitting key when empty. Tests expected it always present. Removed `omitempty`.
- `TestE2E_Sync_DeletePropagation` — immediate `ls` of deleted file failed due to eventual consistency. Replaced with `pollCLIWithConfigNotContains`. Added pre-sync guards for remote deletes.
- `TestE2E_Sync_EditDeleteConflict` — delta endpoint lagged behind REST endpoints (§17). Added delta token advance + retry loop.
- `TestE2E_Sync_EmptyDirectory` — same delta lag pattern. Added delta token advance + retry loop.
- `TestE2E_Sync_NestedDeletion` — same pattern (not in original failure list but same root cause).
- Default drive content (Documents/, Pictures/, "Getting started with OneDrive.pdf") interfered with whole-drive sync tests. Deleted from both test accounts.

---

## 17. Delta Endpoint Lags Behind REST Item Endpoints

**Observed**: After deleting a file via `DELETE /drives/{driveID}/items/{itemID}`, a direct `GET` (or `ls` by path) returns 404 within seconds, but the delta endpoint (`GET /drives/{driveID}/root/delta`) may not include the deletion for several more seconds. This causes tests to pass their poll guards (which use `ls`), then fail when `sync` runs a delta query that doesn't yet reflect the deletion.

**Evidence**:
- `TestE2E_Sync_EditDeleteConflict`: Poll guard confirms `fragile.txt` is gone from `ls`, but the subsequent sync (which uses delta) doesn't see the deletion, so no edit-delete conflict is detected. Nightly CI 2026-03-04, 2026-03-05.
- `TestE2E_Sync_EmptyDirectory`: Poll guard confirms `emptyFolder` is gone from `ls`, but sync's delta doesn't include the folder deletion, so the local folder isn't removed. Nightly CI 2026-03-05.

**Cause**: The delta endpoint aggregates changes from a different consistency domain than the REST item endpoints. REST item endpoints reflect the current state of individual items (direct read from the item's storage node). The delta endpoint aggregates changes from a change log that is populated asynchronously — deletions must propagate from the storage node to the change log before they appear in delta responses. Microsoft documents this: "Due to replication delays, changes to the object do not show up immediately [...] You should retry [...] after some time to retrieve the latest changes." Observed delays range from seconds to over 60 seconds.

**This is distinct from §2 (read-after-write consistency)** — §2 is about REST reads lagging behind REST writes on the same endpoint. This issue is about the delta endpoint (used by sync) lagging behind REST endpoints (used by poll guards).

**Critical constraint — deletions are sent only once**: The delta endpoint delivers deletion events in exactly one response. If the client's token window spans past the deletion, it is permanently missed. The abraunegg/onedrive project maintainer confirms this behavior: deletion events are ephemeral in the change log and have a limited retention window. If the client advances its delta token (even with zero events returned), it may skip over the deletion entirely. A subsequent incremental delta call will never see that deletion again.

This has three implications:
1. Fresh delta (no token) **never** reports deletions — it only enumerates existing items.
2. Incremental delta reports a deletion **exactly once** in the response where the token window covers it.
3. Advancing the token on a zero-event response can permanently skip over pending deletions that haven't propagated to the change log yet.

**Hierarchical deletion behavior**: When a parent folder is deleted, the delta response may only report the parent deletion without individual child item deletions. The client must infer that all descendants are also deleted. Our sync engine handles this correctly — `classifyAndConvert` in `observer_remote.go` processes the parent deletion, and the planner cascades the deletion to all local children.

**How other sync tools handle this**:
- **rclone**: Does NOT use delta for OneDrive sync. It performs full directory tree enumeration every sync cycle. The rclone maintainers explicitly disabled delta support due to the root-only restriction and unreliable deletion tracking. This avoids the problem entirely at the cost of O(n) API calls per sync.
- **abraunegg/onedrive** (v2.5.x): Uses delta but supplements with periodic full scans as a safety net. The v2.5.x release defaulted to download-only mode specifically because auto-deleting local files based on delta deletion events was unreliable — too many false negatives. The maintainer's recommendation is to never trust delta alone for deletions.
- **Microsoft's own recommendation**: "Applications should periodically perform a full delta enumeration to ensure no changes were missed" (paraphrased from Graph API delta docs). No SLA is provided for delta propagation latency.

**Production implication**: Any delta token advancement can potentially skip pending events. This applies to both zero-event responses (most common failure mode) and non-empty batches that exclude still-propagating events.

**Production workaround**: Three-layer defense:
1. **Zero-event guard** (§20): `observeAndCommitRemote()` skips token advancement when delta returns 0 events. The old token replays the same empty window at zero cost.
2. **Full reconciliation** (§21): `sync --full` runs a fresh delta with empty token (enumerates ALL remote items) and detects orphans — baseline entries not in the full enumeration. Periodic full reconciliation in daemon mode (every 24 hours by default).
3. **Retry loops in tests**: 120-second polling with 5-second intervals for deletion-dependent tests.

**Test workaround**: Two-part fix:

1. **Advance delta token** — after the upload sync, run a no-op download sync to ensure the delta token is saved past the creation. This guarantees the subsequent deletion will be reported by incremental delta.
2. **Retry sync in a polling loop** — after poll guards confirm a deletion is visible via `ls`, re-run sync until delta catches up and the expected local-side change materializes. Use `runCLIWithConfigAllowError` inside `require.Eventually` — the condition function runs in a goroutine, and `runCLIWithConfig`'s `require.NoErrorf` would panic if the test times out. Use `--force` to bypass big-delete protection (parallel tests inflate delete counts on shared drives):

```go
// Advance delta token past the creation
runCLIWithConfig(t, cfgPath, env, "sync", "--download-only")
// ... delete remotely ...
pollCLIWithConfigNotContains(t, opsCfgPath, nil, "file.txt", pollTimeout, "ls", "/"+testFolder)
// Retry sync until delta catches up
require.Eventually(t, func() bool {
    _, _, syncErr := runCLIWithConfigAllowError(t, cfgPath, env, "sync", "--download-only")
    if syncErr != nil {
        return false
    }
    _, statErr := os.Stat(localDir)
    return os.IsNotExist(statErr)
}, 120*time.Second, 5*time.Second, "deletion should propagate locally")
```

**De-parallelized tests**: `TestE2E_Sync_DeletePropagation`, `TestE2E_Sync_EmptyDirectory`, `TestE2E_Sync_NestedDeletion`, `TestE2E_Sync_BigDeleteProtection` — these tests depend on delta deletion delivery and run sequentially (no `t.Parallel()`) to avoid cross-test contamination. They use 120s retry loops to handle delta lag.

**Affected tests**: `TestE2E_Sync_EditDeleteConflict` (120s retry timeout).

**Frequency**: Moderate — observed in 2 of the last 5 nightly CI runs.

---

## 18. Dry-Run Must Not Advance Delta Token

**Observed**: `TestE2E_Sync_DryRunNonDestructive` intermittently fails because a dry-run sync observes a remote file via delta and advances the saved delta token, causing the subsequent real sync to miss the same file in its incremental delta response.

**Cause**: `Engine.observeChanges()` called `observeAndCommitRemote()` — which saves both the delta token and remote state observations to SQLite — regardless of the `DryRun` flag. The dry-run returned early (no execution), but the delta token was already persisted. The next real sync used incremental delta from that token, which no longer included the items from the previous response.

**Fix**: When `DryRun` is true, call `observeRemote()` (observe-only, no commit) instead of `observeAndCommitRemote()`. This ensures the delta token and observations remain unchanged after a dry-run, so the next real sync sees the same remote state.

**Test verification**: `TestRunOnce_DryRun_NoExecution` asserts both `baseline.Len() == 0` and `GetDeltaToken() == ""` after a dry-run.

---

## 19. Cross-Test Contamination on Shared Drive

**Observed**: Parallel E2E tests that run bidirectional sync against the same OneDrive account interfere with each other. The delta endpoint returns ALL changes across the entire drive root, so one test's folder creation/deletion can appear as unexpected events in another test's sync cycle.

**Evidence**:
- `TestE2E_Sync_ResolveKeepRemoteThenSync`: Bidirectional sync picks up cleanup deletions from other parallel tests' folders (e.g., `e2e-cli-noconfl/`, `e2e-sync-ver/`), causing `os.Remove` failures on non-empty directories. Nightly CI 2026-03-05.
- `TestE2E_Sync_CrashRecoveryIdempotent`: Re-sync reports "changes detected" instead of "No changes detected" because delta includes items created by other parallel tests that started after the first sync.
- `TestE2E_Sync_BigDeleteProtection`: Parallel tests inflate the baseline item count. When the test expects N items and the big-delete threshold is N×50%, additional items from other tests push the total past the threshold, changing the protection calculation.

**Cause**: All parallel E2E tests share one OneDrive account. The delta endpoint (`GET /drives/{driveID}/root/delta`) returns changes for the **entire drive** — there is no way to scope delta to a subfolder reliably. Folder-scoped delta (`GET /drives/{driveID}/items/{folderId}/delta`) works on OneDrive Personal but broke on OneDrive Business in November 2018 (Microsoft acknowledged, never fixed). Since our CI uses Business accounts, folder-scoped delta is not an option.

**How other sync tools handle test isolation**:
- **rclone**: Uses random-prefix test directories (`rclone-test-XXXXXXXXXXXX`) and full enumeration (no delta). Each test scopes to its own prefix. Since rclone doesn't use delta, cross-test pollution doesn't affect sync correctness — each test only reads its own directory.
- **restic**: Uses a per-test backend factory that creates isolated storage. Tests include `WaitForDelayedRemoval()` helpers for eventual consistency. Each test operates in a completely separate namespace.
- **No project uses delta-based filtering for test isolation** — the root-only nature of the delta endpoint makes this impractical.

**Current mitigations**:
1. Each test creates a uniquely-named folder with a nanosecond timestamp (e.g., `e2e-sync-reskl-1741234567890123456`).
2. Each sync test gets its own `sync_dir` (temp directory) and config file, so local state doesn't overlap.
3. **Deletion-dependent tests run sequentially** (no `t.Parallel()`): `TestE2E_Sync_DeletePropagation`, `TestE2E_Sync_EmptyDirectory`, `TestE2E_Sync_NestedDeletion`, `TestE2E_Sync_BigDeleteProtection`, `TestE2E_BigDeleteProtection`. These tests rely on delta delivering deletion events — cross-test contamination from parallel execution caused spurious failures. Sequential execution eliminates the contamination source while retaining 120s retry loops for delta lag.
4. Upload-only and download-only syncs are less affected (no cross-contamination from other tests' deletions) and keep `t.Parallel()`.

**Remaining exposure**: Bidirectional syncs (`sync --force` without `--upload-only` or `--download-only`) that run in parallel remain vulnerable. The download phase pulls delta events from the entire drive, which may include other tests' folders. Sequential execution for deletion-dependent tests narrows this but does not eliminate it for all bidirectional tests.

**Long-term fix options**:
1. **`remote_path` filtering** (roadmap Phase 10): Configure each sync test to only sync a specific remote subfolder. Delta events outside the configured path are ignored. This doesn't change delta's root-only behavior — it filters events client-side after receiving them. This is the definitive fix.
2. **Separate test accounts per test**: Expensive (token management, Azure AD app registrations) but eliminates the problem entirely.

---

## 20. Delta Token Advancement on Zero-Event Responses

**Observed**: When `observeAndCommitRemote()` calls the delta endpoint and receives zero events but a new delta token, it saves the new token. This advances the client's delta window even though no changes were processed.

**Risk**: If a deletion event hasn't propagated to the delta change log yet (§17) but the client calls delta and gets 0 events + new token, the client's window advances past the pending deletion. When the deletion finally propagates to the change log, it falls outside the client's token window and is permanently missed.

**This is the mechanism behind §17's "deletions sent only once" problem in practice**: The delta endpoint returns a new token even when reporting zero events. The client faithfully saves it. A pending deletion that was still propagating through Microsoft's infrastructure is now stranded behind the saved token.

**Previous behavior**: `observeAndCommitRemote()` always saved the new token regardless of event count. This was correct for the general case (steady-state sync) — the token must advance to avoid reprocessing old events. The problem only manifested when:
1. A mutation (delete) was performed via REST, AND
2. Delta is called before the deletion propagates to the change log, AND
3. The returned token window has advanced past the deletion's change log entry

**Fix implemented**: When delta returns 0 events, the delta token is **not** advanced. Replaying a delta token that returned 0 events costs O(1) — the server already knows there's nothing to return. But if a deletion was still propagating, we haven't advanced past it. This is implemented in both `observeAndCommitRemote()` (one-shot sync) and `RemoteObserver.Watch()` (daemon mode).

This does **not** fully close the window — events can also be excluded from non-empty batches if the token advancement within a batch skips a still-propagating deletion. Full reconciliation (§21) addresses this remaining gap.

**Production impact**: Low in normal operation. The sync daemon calls delta every 30 seconds. If a deletion hasn't propagated within one cycle, it will almost certainly be in the next. The "permanently missed" scenario requires very specific timing. However, in E2E tests where operations happen in rapid succession (delete → poll → sync within seconds), the window is much larger.

**Evidence from other projects**: The abraunegg/onedrive maintainer explicitly warns about this pattern and recommends periodic full scans. rclone avoids the problem entirely by not using delta. Microsoft's docs acknowledge "varying delays" but provide no SLA or guarantee that a single delta call after a mutation will include that mutation.

**Additional mitigation**: Periodic full reconciliation (§21) detects and corrects any items missed by incremental delta.

---

## 21. Full Reconciliation — Orphan Detection

**Problem**: Incremental delta can permanently miss deletion events (§17, §20). The zero-event token guard (§20) narrows the window but doesn't close it — events can also be excluded from non-empty batches if the token advancement within a batch skips a still-propagating deletion.

**Solution**: Full reconciliation runs a fresh delta with an empty token (enumerates ALL remote items) and compares against the baseline to detect **orphans** — items present in the baseline but absent from the full enumeration. These orphans represent remote deletions that were missed by incremental delta.

**Algorithm**:
1. Call `FullDelta(ctx, "")` — enumerates all remote items (returns ChangeCreate/ChangeModify for every item)
2. Collect all seen item IDs into a set
3. Iterate baseline entries via `ForEachPath()` — any entry whose ItemID is NOT in the seen set is an orphan
4. Synthesize `ChangeDelete` events for orphans
5. Feed all events (full enumeration + orphan deletions) through the normal planner + executor pipeline
6. Save the new delta token from the full enumeration

**API cost**: Full enumeration of 100K items requires ~500 API calls (~50 seconds). Per-user rate limit is 3,000 requests / 5 minutes. A full reconciliation consumes ~17% of a single 5-minute rate window. Safe to run periodically (default: every 24 hours in daemon mode).

**Three access modes**:
1. **`sync --full`**: Manual one-shot full reconciliation. Useful for post-incident recovery or after suspected missed deletions.
2. **Daemon mode**: Automatic periodic full reconciliation every `ReconcileInterval` (default 24 hours, configurable). Runs alongside normal incremental delta polling.
3. **Programmatic**: `observeRemoteFull()` and `observeAndCommitRemoteFull()` methods on `Engine` for use by tests or future automation.

**Implementation files**:
- `internal/sync/engine.go`: `observeRemoteFull()`, `observeAndCommitRemoteFull()`, `runFullReconciliation()`, `ReconcileInterval` in `WatchOpts`, reconcile ticker in `RunWatch()`
- `internal/sync/types.go`: `Baseline.FindOrphans()` method
- Root package `sync.go`: `--full` flag, mutual exclusivity with `--watch`

**Relationship to other mitigations**:
- §20 zero-event guard: First line of defense — prevents unnecessary token advancement at zero cost
- §21 full reconciliation: Second line — detects and corrects any orphans that slipped through
- §19 serial test execution: Test-side mitigation — reduces cross-test contamination that amplifies the delta miss window

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
