# Failure & Retry Requirements Matrix

Comprehensive matrix of every failure scenario, whether it self-heals, what the software must do, and what the user must do. Organized by root cause category.

## Legend

- **Scope**: `file` = one item, `subtree` = folder and descendants, `drive` = entire drive, `account` = all drives for one account, `service` = all accounts (Microsoft is down)
- **Self-heals?**: Will it resolve without human intervention?
- **Status**: `implemented`, `planned` (has requirement ID), `gap` (no requirement yet)

---

## 1. Transient Infrastructure Failures

Self-healing failures caused by temporary network or server issues. The software should retry automatically; the user does nothing.

### 1.1 Network Unreachable / DNS Failure / Connection Refused

| Field | Value |
|-------|-------|
| Scope | service |
| Self-heals? | Yes (when network returns) |
| Software action | Retry with exponential backoff. In watch mode, retry forever. In one-shot mode, exhaust finite retries then skip action |
| User action | None (wait for network) |
| Status | Implemented (Transport policy, WatchLocal/WatchRemote policies) |
| Gap | Transport-level retries are finite (5 attempts). In one-shot mode, if all 5 fail, the action is silently skipped with no clear "you're offline" message. Should surface a user-visible network-unreachable signal |

### 1.2 Connection Reset / Broken Pipe Mid-Transfer

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | Yes |
| Software action | Retry HTTP request via `doPreAuthRetry`. Resume download from `.partial` file. Resume upload from persisted session |
| User action | None |
| Status | Implemented |
| Gap | None |

### 1.3 TLS Handshake Failure

| Field | Value |
|-------|-------|
| Scope | service |
| Self-heals? | Usually yes (transient). No if caused by clock skew or corporate MITM proxy |
| Software action | Retry with backoff (treated as network error) |
| User action | None if transient. Fix system clock or proxy config if permanent |
| Status | Implemented (retried as generic network error) |
| Gap | No distinction between transient TLS failures and permanent cert issues. A corporate MITM proxy rejection retries forever in watch mode with no actionable message |

### 1.4 Read Timeout / Stalled Connection (No Data Flowing)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | Yes (if connection eventually fails or resumes) |
| Software action | Kill stalled connection after deadline, retry |
| User action | None |
| Status | Partially implemented. `defaultHTTPClient` has 30s timeout for metadata. `transferHTTPClient` has `Timeout: 0` |
| Requirement | R-6.2.10 [planned] |
| Gap | **High priority.** A stalled transfer connection without context cancellation hangs a worker goroutine indefinitely. Needs per-transfer timeout or connection-level deadline |

### 1.5 HTTP 408 Request Timeout

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | Yes |
| Software action | Retry with backoff |
| User action | None |
| Status | Implemented. Retryable at both graph layer (`isRetryable`) and executor layer (`classifyStatusCode`) |
| Gap | None |

### 1.6 HTTP 500 Internal Server Error

| Field | Value |
|-------|-------|
| Scope | service |
| Self-heals? | Yes |
| Software action | Retry with backoff |
| User action | None |
| Status | Implemented |
| Gap | None |

### 1.7 HTTP 502 Bad Gateway

| Field | Value |
|-------|-------|
| Scope | service |
| Self-heals? | Yes |
| Software action | Retry with backoff |
| User action | None |
| Status | Implemented |
| Gap | None |

### 1.8 HTTP 503 Service Unavailable

| Field | Value |
|-------|-------|
| Scope | service |
| Self-heals? | Yes (when MS recovers) |
| Software action | Retry with backoff. Respect `Retry-After` header if present |
| User action | None |
| Status | Implemented (retryable) |
| Gap | `Retry-After` extraction is only implemented for 429 responses. 503 responses may also carry `Retry-After` but it is not honored |

### 1.9 HTTP 504 Gateway Timeout

| Field | Value |
|-------|-------|
| Scope | service |
| Self-heals? | Yes |
| Software action | Retry with backoff |
| User action | None |
| Status | Implemented |
| Gap | None |

### 1.10 HTTP 509 Bandwidth Limit Exceeded (SharePoint)

| Field | Value |
|-------|-------|
| Scope | drive |
| Self-heals? | Yes (resets over time) |
| Software action | Retry with backoff. Ideally, pause only the affected drive |
| User action | None |
| Status | Implemented (retryable) |
| Gap | No scope awareness — retries files from other drives too, even though only one drive is throttled. Related to planned scope-classified retry (R-2.10.3) |

### 1.11 HTTP 404 Transient (Cross-Datacenter Load Balancer Cache Miss)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | Yes (~1% of requests, succeeds on retry) |
| Software action | Classify as transient, retry with backoff |
| User action | None |
| Requirement | R-6.7.12 [planned] |
| Status | Partially implemented. Graph layer returns `ErrNotFound` for both transient and genuine 404s. Executor re-classifies all 404s as retryable (B-020 workaround) |
| Gap | No distinction at graph level between transient and genuine 404. All 404s are retried at executor level, which is correct for transient but wastes retries on genuinely deleted items |

### 1.12 HTTP 412 Precondition Failed (eTag Race)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | Yes |
| Software action | Retry with backoff (server-side conflict resolves) |
| User action | None |
| Status | Implemented |
| Gap | None |

---

## 2. Rate Limiting

Self-healing after the rate limit window expires. The software must respect server-indicated wait times.

### 2.1 HTTP 429 Too Many Requests (Per-User / Per-App)

| Field | Value |
|-------|-------|
| Scope | account |
| Self-heals? | Yes (after Retry-After period) |
| Software action | Extract `Retry-After` header. Set account-wide throttle gate blocking ALL requests until deadline. `waitForThrottle()` before every request |
| User action | None |
| Status | Implemented |
| Gap | None |

### 2.2 HTTP 429 Per-Tenant (Multi-Drive Aggregate)

| Field | Value |
|-------|-------|
| Scope | account |
| Self-heals? | Yes |
| Software action | Share rate limiter across all drives belonging to the same tenant. One drive hitting 429 should throttle requests for all drives under that tenant |
| User action | None |
| Status | Gap — each drive's `graph.Client` has an independent throttle gate |
| Gap | **Medium priority.** Planned in `graph-client.md`. Multiple drives under the same tenant share Graph API rate limits but have independent throttle gates. Drive A's 429 doesn't slow Drive B, causing cascading throttles across drives |

---

## 3. Extended Outage

Server-side outages lasting minutes to hours. Self-healing eventually but may require hours of patience.

### 3.1 HTTP 400 "ObjectHandle is Invalid" (Microsoft Backend Outage)

| Field | Value |
|-------|-------|
| Scope | service |
| Self-heals? | Yes (after hours — observed: 3.5-hour incident) |
| Software action | Classify as non-retryable (don't hammer a dead API). Report clearly that Microsoft is experiencing an outage. In watch mode, use long probe interval to detect recovery |
| User action | Wait for Microsoft to fix |
| Requirement | R-6.7.14 [planned] |
| Status | Partially handled. 400 → `errClassSkip` → records transient failure → reconciler retries later with backoff |
| Gap | **Medium priority.** No user-visible "Microsoft is experiencing an outage" signal. The specific sub-error (`ObjectHandle is Invalid`) is not distinguished from other 400 errors. In watch mode the reconciler's exponential backoff is reasonable, but in one-shot mode the error message is opaque |

---

## 4. Authentication

Token lifecycle failures. Some self-heal via automatic refresh; others require user re-authentication.

### 4.1 Access Token Expired (Normal Lifecycle)

| Field | Value |
|-------|-------|
| Scope | account |
| Self-heals? | **Yes** (automatic refresh) |
| Software action | Refresh token automatically via `OnTokenChange` callback. Retry the failed request |
| User action | None |
| Status | Implemented (forked `golang.org/x/oauth2` with `OnTokenChange`) |
| Gap | None |

### 4.2 Refresh Token Expired or Revoked

| Field | Value |
|-------|-------|
| Scope | account |
| Self-heals? | **No** |
| Software action | Stop syncing this account. Display clear error: "Authentication expired — run `onedrive login` to re-authenticate." Do not retry auth |
| User action | Run `onedrive login` |
| Status | Implemented. HTTP 401 → `errClassFatal` → aborts sync pass |
| Gap | Error message could be more actionable — explicitly tell user to run `onedrive login` |

### 4.3 App Consent Revoked by User

| Field | Value |
|-------|-------|
| Scope | account |
| Self-heals? | **No** |
| Software action | Same as 4.2 — stop syncing, clear error |
| User action | Re-consent via `onedrive login` |
| Status | Same 401 path as 4.2 |
| Gap | Same as 4.2 — could provide more specific guidance |

### 4.4 Tenant Admin Blocked App

| Field | Value |
|-------|-------|
| Scope | account |
| Self-heals? | **No** |
| Software action | Detect admin-blocked error. Display: "Your organization's administrator has blocked this application. Contact your IT admin." Do not retry |
| User action | Contact tenant administrator |
| Status | Gap — shows generic 403 or 401 error |
| Gap | No parsing of error body for admin-block-specific error codes. Would need to check for `AADSTS65001` or similar codes in the OAuth error response |

### 4.5 Conditional Access Policy (MFA, Device Compliance)

| Field | Value |
|-------|-------|
| Scope | account |
| Self-heals? | **No** |
| Software action | Detect conditional access error. Display policy requirement details if available. Do not retry |
| User action | Comply with policy (enroll device, enable MFA, etc.) |
| Status | Gap — shows generic 403/401 error |
| Gap | Conditional access errors carry specific AAD error codes and `claims` challenge data that could provide actionable guidance. Not parsed |

### 4.6 HTTP 403 Transient After Token Refresh (Eventual Consistency)

| Field | Value |
|-------|-------|
| Scope | account |
| Self-heals? | **Yes** (within seconds) |
| Software action | On 403 from `/me/drives` shortly after token refresh, retry 2-3 times with short backoff before treating as permanent |
| User action | None |
| Requirement | R-6.7.13 [planned] |
| Status | Not implemented. 403 on drive discovery is treated as permanent failure |
| Gap | **Medium priority.** Token propagation in Microsoft's infrastructure has eventual consistency. `/me/drives` can return 403 for seconds after a valid token refresh while `/me` succeeds with the same token |

### 4.7 No Token File / Corrupt Token File

| Field | Value |
|-------|-------|
| Scope | account |
| Self-heals? | **No** |
| Software action | Clear error message: "Not logged in — run `onedrive login`" or "Token file corrupt — run `onedrive login`" |
| User action | Run `onedrive login` |
| Status | Implemented. `tokenfile.Load` returns `ErrNotFound` or parse error. CLI shows appropriate message |
| Gap | None |

---

## 5. Permissions

Permanent failures unless permissions are changed by an external actor (folder owner, admin).

### 5.1 HTTP 403 on Write to Read-Only Shared Folder

| Field | Value |
|-------|-------|
| Scope | subtree |
| Self-heals? | **Maybe** (if sharer grants write access) |
| Software action | Query actual permissions via Graph API. If permanent: record `permission_denied` at boundary folder, suppress all writes to subtree. Cache denied prefixes. Recheck permissions at start of each sync pass — auto-clear if folder becomes writable |
| User action | Ask folder owner for write access, or accept read-only sync |
| Status | Implemented (R-2.14.1). `handle403()` → `walkPermissionBoundary()` → `recheckPermissions()` |
| Gap | None — this is well-implemented |

### 5.2 HTTP 403 on Write — Transient (Server Hiccup)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Yes** |
| Software action | `handle403()` queries actual permissions, finds folder IS writable → falls through to normal transient failure recording with retry |
| User action | None |
| Status | Implemented |
| Gap | None |

### 5.3 Local Filesystem Permission Denied

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **No** |
| Software action | Detect `os.ErrPermission`. Record as actionable issue (not transient). Show in `issues` with specific path and required permissions |
| User action | Fix local permissions (`chmod`/`chown`) |
| Status | Gap — classified as generic transient failure via `errClassSkip` for non-graph errors. Retried forever via reconciler |
| Gap | **High priority.** Local permission denied is retried indefinitely. Should detect `os.ErrPermission` and classify as actionable with a new issue type (e.g., `local_permission_denied`). Retrying won't fix a permission problem |

---

## 6. Storage / Quota

Failures caused by insufficient storage space. Require user action to free space or upgrade.

### 6.1 HTTP 507 Insufficient Storage (OneDrive Quota Exceeded)

| Field | Value |
|-------|-------|
| Scope | account |
| Self-heals? | **No** (until user frees space or upgrades) |
| Software action | Record as actionable `quota_exceeded` issue. Suppress all uploads for the account. Continue downloads and deletes normally. Show in `issues`. Periodic recheck (not exponential backoff — quota doesn't free itself faster with longer waits) |
| User action | Free OneDrive space (delete files, empty recycle bin) or upgrade storage plan |
| Requirement | R-2.10.1 [planned] |
| Status | **Currently `errClassFatal`** — aborts entire sync pass including downloads |
| Gap | **High priority.** Fatal is far too aggressive. A full OneDrive shouldn't prevent downloading remote changes. Should be an actionable issue that suppresses uploads while allowing everything else to proceed. Needs changes in `executor.go` classification, `upload_validation.go`, `baseline.go` schema, and engine |

### 6.2 Local Disk Full

| Field | Value |
|-------|-------|
| Scope | drive |
| Self-heals? | **No** (until user frees space) |
| Software action | Check available disk space before starting download (pre-flight). If insufficient, record as actionable issue. If disk fills mid-download, preserve `.partial` file. Clear error message with required space |
| User action | Free local disk space |
| Requirement | R-6.2.6 [planned] |
| Status | Gap — no pre-download space check. Mid-download `ENOSPC` errors are classified as generic transient failures and retried forever |
| Gap | **Medium priority.** No pre-download disk space check (R-6.2.6). Disk-full errors during download are classified as transient and retried indefinitely. Should detect `ENOSPC` / disk space errors and classify as actionable with a new issue type (e.g., `disk_full`) |

### 6.3 SQLite Database Disk Full

| Field | Value |
|-------|-------|
| Scope | drive |
| Self-heals? | **No** |
| Software action | Handle `SQLITE_FULL` gracefully without corrupting database. Surface clear error |
| User action | Free disk space |
| Status | SQLite handles internally (returns `SQLITE_FULL` error), propagated up |
| Gap | No specific detection or user-friendly message for database-specific disk full |

---

## 7. Content Validation

Failures caused by files that violate OneDrive naming or size rules. Detected before upload.

### 7.1 Invalid OneDrive Filename (Reserved Characters)

Characters: `"`, `*`, `:`, `<`, `>`, `?`, `/`, `\`, `|`

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **No** |
| Software action | Detect in pre-upload validation. Record as actionable `invalid_filename`. Show in `issues` with the specific invalid characters |
| User action | Rename file to remove invalid characters |
| Requirement | R-2.11.1 [planned — pre-upload validation planned, but detection of some chars is implemented] |
| Status | Implemented — `IssueInvalidFilename` in `upload_validation.go` |
| Gap | None for currently validated characters |

### 7.2 Reserved OneDrive Names

Names: `.lock`, `desktop.ini`, `CON`, `PRN`, `AUX`, `NUL`, `COM0`–`COM9`, `LPT0`–`LPT9` (case-insensitive)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **No** |
| Software action | Detect in pre-upload validation. Record as actionable `invalid_filename` |
| User action | Rename file |
| Requirement | R-2.11.2 [planned] |
| Status | Implemented in validation |
| Gap | None for currently validated names |

### 7.3 Reserved OneDrive Patterns

Patterns: names starting with `~$`, names containing `_vti_`, `forms` at root level on SharePoint drives

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **No** |
| Software action | Detect in pre-upload validation. Record as actionable `invalid_filename` |
| User action | Rename file |
| Requirement | R-2.11.3 [planned] |
| Status | Not yet validated |
| Gap | These patterns are not checked in current validation. Files with these names will fail on upload with an opaque API error instead of a clear pre-validation message |

### 7.4 Trailing Dots or Leading/Trailing Whitespace

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **No** |
| Software action | Detect in pre-upload validation. Record as actionable `invalid_filename` |
| User action | Rename file |
| Requirement | R-2.11.4 [planned] |
| Status | Not yet validated |
| Gap | Same as 7.3 — files with trailing dots will fail with opaque API error |

### 7.5 Path Too Long (>400 Characters for OneDrive)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **No** |
| Software action | Detect in pre-upload validation. Record as actionable `path_too_long` |
| User action | Shorten directory structure or file name |
| Status | Implemented — `IssuePathTooLong` |
| Gap | None |

### 7.6 File Too Large (>250 GB)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **No** |
| Software action | Detect in pre-upload validation. Record as actionable `file_too_large` |
| User action | Exclude file or split into smaller files |
| Status | Implemented — `IssueFileTooLarge` |
| Gap | None |

### 7.7 Case Collision (e.g., `file.txt` vs `File.txt` in Same Directory)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **No** |
| Software action | Detect before upload by checking for case-insensitive name collisions in the same directory. Record as actionable issue |
| User action | Rename one of the colliding files |
| Requirement | R-2.12.1 [planned] |
| Status | Not implemented |
| Gap | **Medium priority.** No detection. Upload of a case-colliding name silently overwrites the existing file on OneDrive (OneDrive is case-insensitive). This is a data-loss risk |

### 7.8 Unicode NFC/NFD Mismatch (macOS)

macOS uses NFD normalization for filenames; OneDrive uses NFC.

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Auto-handled** by software |
| Software action | Normalize all filenames to NFC before comparison |
| User action | None |
| Requirement | R-2.13.1 [implemented] |
| Status | Implemented |
| Gap | None |

---

## 8. Transfer Integrity

Failures during file transfer that affect data correctness.

### 8.1 Hash Mismatch After Download

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Yes** (retry usually succeeds) |
| Software action | Delete `.partial` file, re-download from scratch. Max 2 retries (3 total attempts). After exhaustion, accept download with `HashVerified: false` to prevent infinite loops (iOS HEIC quirk) |
| User action | None |
| Status | Implemented — `downloadWithHashRetry` in `transfer_manager.go` |
| Gap | Accepting after retry exhaustion is pragmatic (required for iOS HEIC bug) but the `HashVerified: false` status could be surfaced in `issues` for visibility |

### 8.2 Hash Mismatch After Upload (SharePoint Enrichment)

SharePoint injects metadata into uploaded files, changing the hash and size post-upload.

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Yes** (by design — not a real mismatch) |
| Software action | Detect enrichment. Record server-reported hash as `remote_hash` in baseline (separate from `local_hash`). Do not re-upload. Per-side hash baselines prevent false conflicts |
| User action | None |
| Status | Implemented — upload validation in `upload_validation.go`, per-side hash baselines |
| Gap | None |

### 8.3 No Hash Available on Remote File

Some Business/SharePoint files have no hash values. Zero-byte files consistently have no hash.

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Auto-handled** |
| Software action | Fallback comparison: size + mtime + eTag |
| User action | None |
| Requirement | R-6.7.17 [implemented] |
| Status | Implemented — fallback chain in `driveops/hash.go` |
| Gap | None |

### 8.4 iOS HEIC Metadata Mismatch

`.heic` files uploaded from iOS have metadata (size, hash) that doesn't match the actual download content. API reports original size but serves post-processed version.

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **No** (permanent API bug, filed as onedrive-api-docs#1723) |
| Software action | Accept download after hash retry exhaustion (8.1). Set `HashVerified: false` |
| User action | None (known Microsoft bug) |
| Status | Handled via 8.1's exhaustion path |
| Gap | Could record as a known-quirk issue type for visibility, but functional behavior is correct |

### 8.5 Upload Session Expired (Server-Side, ~15 Minutes)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Yes** (create new session) |
| Software action | Detect 404 on session resume → `ErrUploadSessionExpired` → create fresh upload session |
| User action | None |
| Status | Implemented |
| Gap | None |

### 8.6 HTTP 416 on Upload Resume (Range Not Satisfiable)

When a fragment is partially received but the client doesn't get confirmation, retrying the same range returns 416.

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Yes** (with correct recovery logic) |
| Software action | On 416, query the upload session status endpoint for `nextExpectedRanges`. Resume from the correct byte offset |
| User action | None |
| Requirement | R-5.6.1 [planned] |
| Status | Not implemented. 416 → `ErrRangeNotSatisfiable` → not retried. Upload fails and is retried from scratch via reconciler |
| Gap | **Medium priority.** Current behavior works (retry from scratch) but wastes bandwidth on large files. Should recover mid-session by querying `nextExpectedRanges` |

### 8.7 Download Interrupted Mid-Stream (Ctrl-C / Network Loss)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Yes** (resume from `.partial`) |
| Software action | Preserve `.partial` file on context cancellation. On next run, resume via HTTP Range request from last byte |
| User action | None |
| Status | Implemented — `removePartialIfNotCanceled` preserves `.partial` on `ctx.Canceled` |
| Gap | None |

### 8.8 File Changed Locally During Upload

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Yes** (re-detected on next observation) |
| Software action | On session resume, recompute local file hash. If changed, discard stale session and re-upload new content |
| User action | None |
| Status | Implemented — session store checks `rec.FileHash == localHash` |
| Gap | None |

### 8.9 File Deleted Locally During Download

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Yes** (re-planned on next observation) |
| Software action | Next local observation detects the deletion, planner creates the appropriate action |
| User action | None |
| Status | Handled via normal observation/planning cycle |
| Gap | None |

---

## 9. Sync Conflicts

Multiple parties modify the same item concurrently.

### 9.1 Both Sides Modified Same File

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Auto-resolved** (keep both by default) |
| Software action | Remote version wins the original path. Local version renamed to `<name>.conflict-<timestamp>.<ext>`. Conflict recorded in `conflicts` table. Visible in `issues` |
| User action | Optional: `issues resolve <path>` to choose `--keep-local`, `--keep-remote`, or `--keep-both` |
| Status | Implemented (R-2.3.1) |
| Gap | Sub-second uniqueness: second-precision timestamps mean two conflicts in the same second produce the same `.conflict-*` name. Planned fix in sync-execution.md |

### 9.2 Edit-Delete Conflict (Local Edit, Remote Delete)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Auto-resolved** |
| Software action | Local modified version wins — uploaded to remote. Conflict recorded as `resolved_by: "auto"` |
| User action | None |
| Status | Implemented |
| Gap | None |

### 9.3 HTTP 409 Conflict (Name Collision on Create)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | Depends on cause |
| Software action | Currently classified as `errClassSkip`. Could be smarter: if creating a folder that already exists server-side, treat as success |
| User action | None in most cases |
| Status | Implemented (skip) |
| Gap | Folder-create 409 could be treated as idempotent success rather than a skipped failure |

### 9.4 Big-Delete Threshold Exceeded

| Field | Value |
|-------|-------|
| Scope | drive |
| Self-heals? | **No** (safety gate requires explicit user acknowledgment) |
| Software action | Abort sync. Display count of planned deletions vs threshold. Require `--force` to proceed |
| User action | Review planned deletions, then run with `--force` if correct |
| Status | Implemented (R-6.4.1, R-6.4.2). Both absolute (default: 1000) and percentage (default: 50%) thresholds. Global and per-folder |
| Gap | None |

---

## 10. Delta / Observation Edge Cases

Failures in the change detection pipeline.

### 10.1 HTTP 410 Delta Token Expired

| Field | Value |
|-------|-------|
| Scope | drive |
| Self-heals? | **Yes** (full re-enumeration) |
| Software action | Discard expired delta token, perform full re-enumeration from scratch |
| User action | None |
| Requirement | R-6.7.5 [implemented] |
| Status | Implemented — `ErrGone` sentinel triggers full resync |
| Gap | None |

### 10.2 Zero-Event Delta Response (Token Advance Risk)

If the client saves a new delta token from a zero-event response, ephemeral deletion events that haven't propagated yet are permanently missed.

| Field | Value |
|-------|-------|
| Scope | drive |
| Self-heals? | **No** (deletions are permanently missed) |
| Software action | Do not advance delta token when response contains zero events. Rely on periodic full reconciliation as a safety net |
| User action | None |
| Requirement | R-6.7.19 [planned] |
| Status | Gap — unclear if token is advanced on zero events in current implementation |
| Gap | **Medium priority.** Combined with delta consistency lag (5-60+ seconds), this can cause permanent orphaned baseline entries. Periodic 24h reconciliation catches these, but the window is long |

### 10.3 Missed Delta Deletion (Orphaned Baseline Entry)

A deletion event was missed (see 10.2) and the baseline still has the item.

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Yes** (periodic reconciliation detects orphans) |
| Software action | Periodic full reconciliation (default: every 24h in watch mode) enumerates all items and detects baseline entries with no server-side counterpart |
| User action | None |
| Requirement | R-2.8.4 [implemented] |
| Status | Implemented |
| Gap | 24h is a long window. One-shot sync doesn't have periodic reconciliation — missed deletions persist until the next `sync --full` |

### 10.4 Phantom System Drives (Personal Accounts)

Personal accounts have 2-3 hidden system drives (face crops, albums) that return HTTP 400 on access.

| Field | Value |
|-------|-------|
| Scope | account |
| Self-heals? | **No** (permanent Microsoft behavior) |
| Software action | For Personal accounts, use `GET /me/drive` (singular) to discover the primary drive instead of `GET /me/drives` (plural). Filter out phantom drives |
| User action | None |
| Requirement | R-6.7.11 [planned] |
| Status | Not implemented |
| Gap | Phantom drives may cause errors or confusion during drive discovery |

### 10.5 OneNote Package Items (No File/Folder Facet)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **N/A** (permanent item property) |
| Software action | Filter in normalization pipeline |
| User action | None |
| Status | Implemented (R-6.7.9) |
| Gap | None |

### 10.6 SharePoint Co-Authoring Lock (HTTP 423)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Yes** (when user closes the file, typically minutes to hours) |
| Software action | Classify as skip (`errClassSkip`). Do not retry with backoff (locks can persist for hours, retrying wastes resources). Watch mode picks up the item on next safety scan |
| User action | None |
| Status | Implemented (B-020) |
| Gap | None |

### 10.7 Delta Consistency Lag (Changes Not Yet Visible)

After a mutation, the delta endpoint may not include the change for 5-60+ seconds.

| Field | Value |
|-------|-------|
| Scope | drive |
| Self-heals? | **Yes** (seconds to minutes) |
| Software action | Periodic reconciliation catches anything missed. In one-shot mode, next run picks it up |
| User action | None |
| Status | Mitigated by 24h full reconciliation |
| Gap | In one-shot mode with no reconciliation, changes during the lag window are missed until next `sync` invocation. Acceptable trade-off |

---

## 11. Local Filesystem Failures

Errors from the local operating system during file operations.

### 11.1 Local File Locked by Another Process

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **Maybe** (when the other process releases the lock) |
| Software action | Record as transient failure, retry with backoff |
| User action | None (usually) |
| Status | Classified as generic transient failure, retried via reconciler |
| Gap | None — transient classification is correct here since locks are temporary |

### 11.2 Local Permission Denied (Read — Can't Upload)

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **No** |
| Software action | Detect `os.ErrPermission`. Record as actionable issue with type `local_permission_denied`. Show in `issues` with path |
| User action | Fix permissions: `chmod` / `chown` |
| Status | Gap — classified as generic transient failure, retried forever |
| Gap | **High priority.** See 5.3. Retrying permission denied is pointless. Needs `os.ErrPermission` detection → actionable issue type |

### 11.3 Local Permission Denied (Write — Can't Download Into Sync Dir)

| Field | Value |
|-------|-------|
| Scope | file or drive (if sync root is unwritable) |
| Self-heals? | **No** |
| Software action | Same as 11.2 |
| User action | Fix permissions on sync directory |
| Status | Same gap as 11.2 |
| Gap | Same as 11.2 |

### 11.4 Sync Directory Unmounted / Disappeared

| Field | Value |
|-------|-------|
| Scope | drive |
| Self-heals? | **No** |
| Software action | Detect that sync root is missing. Abort sync for safety to prevent interpreting "everything is gone" as "delete everything remote" (R-6.2.2) |
| User action | Remount volume or fix config |
| Status | Implemented — incomplete enumeration detection prevents mass deletion |
| Gap | None |

### 11.5 Symlink Escape from Sync Directory

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **N/A** (security protection, always blocked) |
| Software action | Reject via `containedPath()` + `filepath.EvalSymlinks` on parent directory |
| User action | None |
| Status | Implemented |
| Gap | None |

### 11.6 Path Too Long for Local OS

| Field | Value |
|-------|-------|
| Scope | file |
| Self-heals? | **No** |
| Software action | Detect `ENAMETOOLONG` / path length errors. Record as actionable issue |
| User action | Shorten path (may require action on remote side) |
| Status | Gap — generic error, no specific detection |
| Gap | No specific detection for OS-level path length errors. Would benefit from a `local_path_too_long` issue type |

### 11.7 Disk Full During Download

See 6.2 (Local Disk Full). Duplicated here for completeness of the filesystem section.

---

## 12. Configuration Errors

Failures due to invalid or missing configuration.

### 12.1 Config File Missing

| Field | Value |
|-------|-------|
| Scope | global |
| Self-heals? | **No** |
| Software action | Clear error message guiding user to `onedrive setup` or config creation |
| User action | Create config file |
| Status | Implemented |
| Gap | None |

### 12.2 Config File Invalid TOML

| Field | Value |
|-------|-------|
| Scope | global |
| Self-heals? | **No** |
| Software action | Parse error with line number |
| User action | Fix config syntax |
| Status | Implemented |
| Gap | None |

### 12.3 Drive Section References Non-Existent Account

| Field | Value |
|-------|-------|
| Scope | drive |
| Self-heals? | **No** |
| Software action | Clear error at startup |
| User action | Fix config (add account or fix reference) |
| Status | Implemented |
| Gap | None |

---

## 13. Planned: Scope-Classified Retry

Cross-cutting concern that affects all retry behavior. Currently all retries use the same backoff curve regardless of failure scope.

### 13.1 File-Scoped Retry

| Field | Value |
|-------|-------|
| Applies to | Failures affecting a single file (hash mismatch, 404, 412, file lock, etc.) |
| Backoff curve | Immediate exponential — short initial delay, moderate max |
| Rationale | Problem is isolated. Aggressive retry is safe and efficient |
| Requirement | R-2.10.3 [planned] |

### 13.2 Drive-Scoped Retry

| Field | Value |
|-------|-------|
| Applies to | Failures affecting one drive (509 bandwidth, SharePoint-specific errors) |
| Backoff curve | Probe-based — check if drive is accessible before retrying affected items |
| Rationale | Don't hammer all files when the drive itself is the problem |
| Requirement | R-2.10.3 [planned] |

### 13.3 Service-Wide Retry

| Field | Value |
|-------|-------|
| Applies to | Failures affecting all accounts (500-504, ObjectHandle is Invalid, network down) |
| Backoff curve | Long delay with single probe — test one lightweight request, resume all on success |
| Rationale | Don't retry hundreds of items individually when the entire service is down |
| Requirement | R-2.10.3 [planned] |

### 13.4 Account-Wide Retry

| Field | Value |
|-------|-------|
| Applies to | Failures affecting one account (429, 507, auth issues) |
| Backoff curve | Account-level gate — block all requests for the account until condition clears |
| Rationale | Per-account issues (quota, rate limit) shouldn't affect other accounts |
| Requirement | R-2.10.3 [planned] |

### 13.5 Status Display with Scope Context

| Field | Value |
|-------|-------|
| Applies to | `status` and `issues` commands |
| Behavior | Display failure scope (file, drive, account, service) alongside retry information |
| Requirement | R-2.10.4 [planned] |

---

## Gap Priority Summary

### High Priority

| # | Gap | Impact |
|---|-----|--------|
| 6.1 | HTTP 507 is fatal instead of actionable | Aborts entire sync pass — stops downloads even though only uploads are blocked |
| 5.3 / 11.2 | Local permission denied retried forever | Pointless infinite retries with no user signal. Needs `os.ErrPermission` → actionable |
| 1.4 | Stalled transfer connection hangs forever | Blocks a worker goroutine indefinitely. R-6.2.10 [planned] |
| 13.x | No scope-classified retry | File, drive, and service failures all use same backoff. Service-down causes N parallel retries instead of 1 probe |

### Medium Priority

| # | Gap | Impact |
|---|-----|--------|
| 4.6 | Transient 403 after token refresh not retried | Drive discovery fails unnecessarily. R-6.7.13 [planned] |
| 8.6 | Upload 416 not recovered mid-session | Wastes bandwidth re-uploading from scratch. R-5.6.1 [planned] |
| 6.2 | Disk full retried forever | No pre-download space check, disk-full errors classified as transient. R-6.2.6 [planned] |
| 2.2 | No per-tenant rate coordination | Multi-drive accounts cause cascading 429s |
| 10.2 | Zero-event delta token advance risk | Ephemeral deletions can be permanently missed. R-6.7.19 [planned] |
| 10.4 | Phantom system drives not filtered | Errors during Personal account drive discovery. R-6.7.11 [planned] |
| 7.7 | No case collision detection | Silent data loss on case-insensitive OneDrive. R-2.12.1 [planned] |
| 3.1 | No "Microsoft outage" detection | Opaque error messages during multi-hour outages. R-6.7.14 [planned] |

### Low Priority

| # | Gap | Impact |
|---|-----|--------|
| 1.1 | No "you're offline" signal in one-shot mode | Actions silently skipped after 5 transport retries |
| 7.3 / 7.4 | `~$` and trailing dot validation missing | Opaque API errors instead of clear pre-validation. R-2.11.3, R-2.11.4 [planned] |
| 4.4 / 4.5 | No admin-block or conditional access detection | Generic 403/401 instead of actionable guidance |
| 1.8 | 503 Retry-After not honored | Only 429 extracts Retry-After header |
| 11.6 | No OS path-too-long detection | Generic error instead of actionable issue |
| 8.1 | `HashVerified: false` not surfaced in issues | Silent acceptance of unverified downloads |
| R-2.10.2 | No auto-resolution detection for actionable issues | User fixes bad filename, failure entry lingers |
