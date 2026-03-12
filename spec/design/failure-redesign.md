# Failure Handling Redesign — Analysis & Recommendations

Comprehensive analysis addressing all questions, corrections from code review, three-alternative designs for each architectural decision, to-be journeys, and full requirements inventory.

---

## Failure Taxonomy

Structured reference for every failure type in the system. Each table captures one dimension. Together they are the complete specification — the rest of this document is analysis and rationale.

**How to verify completeness:** Every `issue_type` in Table 1 must have exactly one row in Tables 2, 3, and 4. Every scope key in Table 1 must appear in Table 5. A missing cell is a spec gap.

**Scope key format convention:** `ScopeState` (in-memory, used for scope matching in the tracker) uses stable internal identifiers: `quota:shortcut:$remoteDrive:$remoteItem`. The `sync_failures` table (persistent, used for `issues` display) uses human-readable local paths: `quota:shortcut:Team Docs`. Translation happens once when recording a failure — the engine knows both. Tables below show the human-readable format.

### Table 1: Error Classification

What each error is, how it's classified, and what scope it maps to.

| Issue Type | Trigger | Classification | Scope Key | Scope Level | Blocks | Requirements |
|---|---|---|---|---|---|---|
| `invalid_filename` | Scanner: `isValidOneDriveName` rejects | actionable | — (file-scoped) | File | Nothing (file excluded from plan) | R-2.11.1–5 |
| `path_too_long` | Scanner: path > 400 chars | actionable | — (file-scoped) | File | Nothing (file excluded from plan) | R-2.11.5 |
| `file_too_large` | Scanner: file > 250 GB | actionable | — (file-scoped) | File | Nothing (file excluded from plan) | R-2.11.5 |
| `case_collision` | Scanner: case-insensitive name conflict in same directory | actionable | — (file-scoped) | File | Nothing (both files excluded) | R-2.12.1–2 |
| `quota_exceeded` | HTTP 507 on own-drive upload | actionable | `quota:own` | Account (own drives) | Own-drive uploads only | R-2.10.1, R-2.10.19 |
| `quota_exceeded` | HTTP 507 on shortcut upload | actionable | `quota:shortcut:{localPath}` | Per-shortcut | Uploads to that shortcut only | R-2.10.1, R-2.10.20 |
| `permission_denied` | HTTP 403 on own-drive action | actionable | `perm:remote:{localPath}` | Folder subtree (own drive) | All remote writes under boundary | R-2.10.23 |
| `permission_denied` | HTTP 403 on shortcut action | actionable | `perm:remote:{localPath}` | Folder subtree (per-shortcut) | All remote writes under that shortcut | R-2.10.23–25 |
| `local_permission_denied` | `os.ErrPermission` on file (dir readable) | actionable | — (file-scoped) | File | Nothing (file fails individually) | R-2.10.12 |
| `local_permission_denied` | `os.ErrPermission` on file (dir unreadable) | actionable | `perm:dir:{path}` | Directory subtree | All ops under directory | R-2.10.12–13 |
| `disk_full` | Disk available < `min_free_space` | actionable | `disk:local` | Local system | All downloads | R-2.10.43, R-6.2.6 |
| `file_too_large_for_space` | Disk ≥ `min_free_space` but < file size + reserve | actionable | — (file-scoped) | File | Nothing (smaller files may proceed) | R-2.10.44 |
| `rate_limited` | HTTP 429 (any request) | transient | `throttle:account` | Account (all drives) | All actions, all drives + shortcuts | R-2.10.26, R-6.8.4 |
| `service_outage` | HTTP 5xx (pattern: 5 unique paths / 30s) | transient | `service` | Service (all drives) | All actions, all drives + shortcuts | R-2.10.28 |
| `service_outage` | HTTP 503 with `Retry-After` | transient | `service` | Service (all drives) | All actions, all drives + shortcuts | R-2.10.28 |
| `service_outage` | HTTP 400 with `innerError.code == "invalidRequest"` and known outage body (e.g., "ObjectHandle is Invalid") (disambiguated from phantom drive 400s (R-6.7.11) by error body inspection — phantom drives fail on one specific drive ID; outage 400s affect all endpoints) | transient | `service` | Service (all drives) | All actions, all drives + shortcuts | R-6.7.14 |
| `auth_expired` | HTTP 401 after token refresh fails | fatal | — | Session | Sync terminates | R-6.8.14 |
| `server_error` | HTTP 5xx (individual, before scope triggers) | transient | — (file-scoped) | File | Nothing (retried via sync_failures + reconciler) | R-6.8.10 |
| `request_timeout` | HTTP 408 (individual) | transient | — (file-scoped) | File | Nothing (retried via sync_failures + reconciler) | R-6.8.10 |
| `transient_conflict` | HTTP 412 (individual) | transient | — (file-scoped) | File | Nothing (retried via sync_failures + reconciler) | R-6.8.10 |
| `transient_not_found` | HTTP 404 (individual, resolves after delta reordering) | transient | — (file-scoped) | File | Nothing (retried via sync_failures + reconciler) | R-6.8.10 |
| `resource_locked` | HTTP 423 (SharePoint co-authoring lock) | transient | — (file-scoped) | File | Nothing (retried via sync_failures + reconciler) | R-6.8.10 |
| — (shutdown) | context.Canceled or context.DeadlineExceeded | — | — | Session | Nothing (action discarded, not recorded) | R-2.8.3 |

Note: context.Canceled/DeadlineExceeded actions are discarded during graceful shutdown — they do not appear in Tables 2, 3, or 4 because no sync_failures entry is created. See R-2.8.3 (two-signal shutdown).

Note: `permission_denied` blocks "all remote writes" — uploads, remote deletes, remote moves, and remote folder creates. Downloads and local-only operations (local deletes, local moves) continue. See Table 5 for the full matrix.

### Table 2: Detection & Recovery

How each failure is detected and how the system recovers. The "Mechanism" column distinguishes three fundamentally different lifecycle types.

| Scope Key | Mechanism | Detection | Threshold | Recovery | First Trial | Backoff | Max Interval | Persist Scope? |
|---|---|---|---|---|---|---|---|---|
| — (file-scoped, observation) | Scanner filter | Scanner walk | Immediate | Auto-clear when file renamed/deleted/excluded | — | — | — | sync_failures only |
| — (file-scoped, transient) | ScopeState + sync_failures | Single HTTP error | Immediate | sync_failures with next_retry_at (1s, 2s, 4s... max 1h). Reconciler re-injects via buffer→planner→tracker. | — | — | — | sync_failures immediately |
| `quota:own` | ScopeState + trial | Pattern | 3 unique own-drive paths with 507 in 10s | Trial (real upload to own drive) | 5 min | 2× | 1 hour | No (rediscover: 3 requests) |
| `quota:shortcut:{d}:{i}` | ScopeState + trial | Pattern | 3 unique paths to same shortcut with 507 in 10s | Trial (real upload to that shortcut) | 5 min | 2× | 1 hour | No |
| `perm:remote:*` | permissionCache + recheck | `handle403` + `walkPermissionBoundary` | Single 403 + boundary walk | Queryable: `recheckPermissions()` each sync pass | — | — | — | Via sync_failures |
| `perm:dir:{path}` | permissionCache + recheck | `os.ErrPermission` + parent dir check | Single error (deterministic) | Recheck `os.Open(dir)` each sync pass | — | — | — | Via sync_failures |
| `disk:local` | ScopeState + trial | Executor pre-check: available < `min_free_space` | Single check (deterministic) | Trial (real download, pre-check re-runs) | 5 min | 2× | 1 hour | No |
| `throttle:account` | ScopeState + trial | Server signal | Single 429 response | Trial (any action) | `Retry-After` from response | 2× | 10 min | No (expires by restart) |
| `service` | ScopeState + trial | Pattern (or server signal for 503 w/ Retry-After, 400 outage) | 5 unique paths with 5xx in 30s (pattern); single response (server signal) | Trial (any action) | 60s (or `Retry-After` if present) | 2× | 10 min | No (transient by nature) |

### Table 3: User Communication

What the user sees per failure type. Scope variants get separate rows.

| Issue Type | Scope Variant | Reason Text | Action Text |
|---|---|---|---|
| `invalid_filename` | — | "File name '{name}' contains characters not allowed on OneDrive ({chars})" or "reserved name" | "Rename the file, or add it to skip_files in config" |
| `path_too_long` | — | "Path exceeds OneDrive's 400-character limit ({len} characters)" | "Shorten the directory structure or file name" |
| `file_too_large` | — | "File exceeds OneDrive's 250 GB size limit ({size})" | "Exclude via skip_files in config" |
| `case_collision` | — | "Files '{name1}' and '{name2}' differ only in case — OneDrive is case-insensitive" | "Rename one of the files to avoid the collision" |
| `quota_exceeded` | Own drive | "Your OneDrive storage is full ({used}/{total})" | "Free space on OneDrive (delete files, empty recycle bin) or upgrade storage plan" |
| `quota_exceeded` | Shortcut | "Shared folder '{name}' owner's storage is full" | "Contact the folder owner to free space or upgrade their storage plan" |
| `permission_denied` | Own drive | "OneDrive folder '{boundary}' is read-only" | "Request write access from the folder owner, or accept read-only sync" |
| `permission_denied` | Shortcut | "Shared folder '{name}' is read-only (shared without write access)" | "Request write access from '{owner}', or accept read-only sync for this folder" |
| `local_permission_denied` | File | "Cannot read file (local permission denied)" | "Fix file permissions: chmod/chown the file or its parent directory" |
| `local_permission_denied` | Directory | "Directory '{path}' is not readable" | "Fix directory permissions: chmod/chown" |
| `disk_full` | — | "Local disk space below minimum ({available} free, {min_free_space} required)" | "Free local disk space. Downloads will resume automatically." |
| `file_too_large_for_space` | — | "Insufficient space for file: need {size}, available {available}" | "Free local disk space or increase min_free_space to 0 to disable reservation" |
| `rate_limited` | — | "OneDrive API rate limit reached (retry in {seconds}s)" | "No action needed — will auto-resolve. Reduce transfer_workers to lower request rate" |
| `service_outage` | — | "Microsoft Graph API appears unavailable (HTTP {code})" | "No action needed — will auto-resolve when service recovers" |
| `auth_expired` | — | "Authentication has expired" | "Run 'onedrive login' to re-authenticate" |
| `server_error` | — | "Server error (HTTP {code})" | "Will auto-resolve — no action needed. If persistent, check Microsoft service status" |
| `request_timeout` | — | "Request timed out (HTTP 408)" | "Will auto-resolve — no action needed" |
| `transient_conflict` | — | "Conflict detected (HTTP 412)" | "Will auto-resolve — no action needed" |
| `transient_not_found` | — | "Item not found (HTTP 404) — may resolve after sync catches up" | "Will auto-resolve — no action needed" |
| `resource_locked` | — | "File locked by SharePoint co-authoring (HTTP 423)" | "Will auto-resolve when lock is released, or close the file in the other application" |

### Table 4: Auto-Clear Rules

When each failure type is automatically removed without user intervention.

| Issue Type | Auto-Clear Condition | Mechanism | Requirement |
|---|---|---|---|
| `invalid_filename` | File renamed/deleted/excluded | Engine compares `ScanResult.Skipped` against sync_failures after each scan | R-2.10.2 |
| `path_too_long` | Path shortened or file deleted | Same as above | R-2.10.2 |
| `file_too_large` | File deleted or excluded via skip_files | Same as above | R-2.10.2 |
| `case_collision` | One of the colliding files renamed | Same as above | R-2.10.2 |
| `quota_exceeded` (own drive) | Trial upload to own-drive path succeeds | Scope block `quota:own` cleared; action success clears sync_failures entry | R-2.10.5, R-2.10.21, R-2.10.41 |
| `quota_exceeded` (shortcut) | Trial upload to that specific shortcut succeeds | Scope block `quota:shortcut:*` cleared; action success clears sync_failures entry | R-2.10.5, R-2.10.21, R-2.10.41 |
| `permission_denied` | `recheckPermissions()` finds folder writable | Permission cache refreshed each sync pass; sync_failures cleared | R-2.10.9 |
| `local_permission_denied` (dir) | `os.Open(dir)` succeeds on next sync pass | Engine `recheckLocalPermissions()` clears entry + scope block | R-2.10.13 |
| `local_permission_denied` (file) | File permissions fixed, next upload succeeds | Action success clears sync_failures entry | R-2.10.41 |
| `disk_full` | Trial download's pre-check finds space available | Scope block cleared | R-2.10.43 |
| `file_too_large_for_space` | Disk space freed, next download attempt fits | Action success clears sync_failures entry | R-2.10.41 |
| `rate_limited` | Trial succeeds after `Retry-After` | Scope block cleared (no sync_failures entry for transient) | R-2.10.7 |
| `service_outage` | Trial succeeds | Scope block cleared | R-2.10.8 |
| `auth_expired` | Never (fatal) | User must run `onedrive login` | — |
| `server_error` | Retry succeeds | Reconciler retry succeeds, sync_failures entry cleared | R-6.8.10, R-2.10.41 |
| `request_timeout` | Same as `server_error` | Same as above | R-6.8.10, R-2.10.41 |
| `transient_conflict` | Same as `server_error` | Same as above | R-6.8.10, R-2.10.41 |
| `transient_not_found` | Same as `server_error` | Same as above | R-6.8.10, R-2.10.41 |
| `resource_locked` | Same as `server_error` (lock releases, retry succeeds) | Same as above | R-6.8.10, R-2.10.41 |

### Table 5: Scope Interaction with Shared Drives

How each scope type behaves across own-drive vs shortcut boundaries within a single engine. "Remote writes" = uploads, remote deletes, remote moves, remote folder creates.

| Action | 429 blocks? | 5xx blocks? | Own-drive 507 blocks? | Shortcut X 507 blocks? | Own-drive 403 blocks? | Shortcut X 403 blocks? |
|---|---|---|---|---|---|---|
| Own-drive uploads | Yes | Yes | **Yes** | No | If under boundary | No |
| Own-drive remote deletes | Yes | Yes | No | No | If under boundary | No |
| Own-drive remote moves | Yes | Yes | No | No | If under boundary | No |
| Own-drive downloads | Yes | Yes | No | No | No | No |
| Own-drive local deletes | Yes | Yes | No | No | No | No |
| Own-drive local moves | Yes | Yes | No | No | No | No |
| Shortcut X uploads | Yes | Yes | No | **Yes** | No | If under boundary |
| Shortcut X remote deletes | Yes | Yes | No | No | No | If under boundary |
| Shortcut X remote moves | Yes | Yes | No | No | No | If under boundary |
| Shortcut X downloads | Yes | Yes | No | No | No | No |
| Shortcut X local deletes | Yes | Yes | No | No | No | No |
| Shortcut Y uploads | Yes | Yes | No | No | No | No |
| Shortcut Y remote deletes | Yes | Yes | No | No | No | No |
| Shortcut Y remote moves | Yes | Yes | No | No | No | No |
| Shortcut Y downloads | Yes | Yes | No | No | No | No |
| Shortcut Y local deletes | Yes | Yes | No | No | No | No |
| Observation polling | Yes (R-2.10.30) | Yes (R-2.10.30) | No | No (R-2.10.31) | No | No |

Note: `disk:local` scope blocks all downloads (own-drive, all shortcuts) but does not block uploads, deletes, moves, or observation polling. See R-2.10.43.

### Table 6: Architectural Removals

What gets deleted from the old architecture. Ensures no remnants.

| What | Where | Replaced By | Requirement |
|---|---|---|---|
| `withRetry()` loop | `sync/executor.go` | Engine classification + sync_failures + reconciler | R-6.8.9 |
| `classifyError()` | `sync/executor.go` | Engine `classifyResult()` | R-6.8.9 |
| `classifyStatusCode()` | `sync/executor.go` | Engine `classifyResult()` | R-6.8.9 |
| `sleepFunc` field | `sync/executor.go` | Non-blocking sync_failures + reconciler retry | R-6.8.7 |
| `errClassRetryable/Fatal/Skip` | `sync/executor.go` | Engine `resultClass` enum | R-6.8.9 |
| `executorMaxRetries` const | `sync/executor.go` | Reconciler backoff (sync_failures `next_retry_at`) | R-6.8.10 |
| `retry.Action` policy | `internal/retry/named.go` | Reconciler backoff schedule (1s→1h) | R-6.8.10 |
| `circuit.go` + test | `internal/retry/` | Scope-based blocking with trial actions | — (dead code) |
| Circuit breaker docs | `internal/retry/doc.go` | Remove section (references deleted code) | — |
| `FailureRecorder` interface | `sync/store_interfaces.go` | Consolidated into `SyncFailureRecorder` | — (consolidation) |
| `RecordFailure` method | `sync/baseline.go` | `RecordSyncFailure` (all callers) | — (consolidation) |
| Transport retry in sync path | `graph/client.go` | `SyncTransport` policy (MaxAttempts: 0) | R-6.8.8 |
| `isValidOneDriveName()` call | `sync/observer_remote.go:405` | Nothing (wrong: upload rules on downloads) | — (bug fix) |
| 423 `errClassSkip` handling | `sync/executor.go` | Classified as transient in engine `classifyResult()` | R-6.8.9 |
| `remote_state` failure transitions | `sync/baseline.go` (inside RecordFailure) | Moved to CommitOutcome (symmetry: success + failure state transitions in one place) | — (consolidation) |

---

## Part 1: Corrections to As-Is Journeys

### Correction 1: Retry-After IS properly parsed

The as-is journey for 429 was misleading. `retryBackoff` (client.go:407–425) parses the actual `Retry-After` header value from the server using `strconv.Atoi(ra)` and uses that exact duration — it does NOT default to 30 seconds. The 30s in the journey was an example of what the server might send. The code is correct: server says how long to wait, we wait exactly that long, AND we set `throttledUntil` to gate all subsequent requests.

### Correction 2: Upload success DOES clear sync_failures

`CommitOutcome` → `updateRemoteStateOnOutcome` (baseline.go:545–565) executes `DELETE FROM sync_failures WHERE path = ?` for `ActionUpload` and `ActionFolderCreate` successes. So Journeys 1/4/5/6 (upload-side failures) DO auto-clear on success. However, download and delete successes do NOT auto-clear. This is a partial implementation.

### Correction 3: Scanner-filtered files are invisible to the engine

Files filtered by `isValidOneDriveName` in the scanner (scanner.go:271) never enter the change pipeline. The engine never sees them. No `sync_failures` entry is created. The only trace is a DEBUG-level log line. This means the user has **no user-facing signal** that files are being skipped due to invalid names — `onedrive issues` shows nothing.

---

## Part 2: isValidOneDriveName Duplication

**Is there duplication?** Yes, technically — the same function is called in two places:

1. **Scanner** (scanner.go:271): Filters during local observation. Files with invalid names never become `ChangeEvent`s.
2. **Upload validation** (upload_validation.go:74): Checks during pre-upload validation in `filterInvalidUploads`.

**Does the engine see scanner-filtered files?** No. The scanner is the first gate. If a file has an invalid name, it's skipped at scanner.go:271 with only a DEBUG log. No `ChangeEvent` is created, no action is planned, `filterInvalidUploads` never sees it, and no `sync_failures` entry exists.

**So what does `filterInvalidUploads` actually catch?** Only files that:
- Were renamed between scanner pass and execution (rare race condition)
- Arrived via the reconciler's synthetic `ChangeEvent` (retry of a previously-valid file that was renamed to an invalid name — an edge case of an edge case)

**Problem:** The dual-gate creates a confusing behavior gap:
- Files filtered by the scanner: invisible, DEBUG log only, not in `issues`
- Files filtered by validation: visible in `issues` as actionable failures

**Recommendation:** Unify. The scanner should record a `sync_failures` entry with `category='actionable'` for files it skips due to invalid names. This gives the user a complete picture. The upload validation remains as a safety net (defense in depth, consistent with the philosophy of "extremely robust and full of defensive coding practices").

---

## Part 3: Batched Warnings & Issues Display

### Logging: Aggregate warnings when count > threshold

Currently, 100k invalid files produce 100k individual WARN log lines. This overwhelms logs and is unhelpful.

**Proposed requirement (R-6.6.7):** When more than 10 items trigger the same warning category (same issue_type, same HTTP status, same error class) within a single sync pass, the system shall aggregate them into a single summary log line at WARN level, with individual items logged at DEBUG level.

Example: instead of 100k lines of `"pre-upload validation failed" path="CON.txt" issue_type="invalid_filename"`, emit:
- 1× WARN: `"100,000 files rejected by pre-upload validation" invalid_filename=95000 path_too_long=5000`
- 100k× DEBUG: individual paths (only visible in debug mode)

### Issues command: Group and paginate

**Proposed requirement (R-2.3.7):** When the `issues` command encounters more than 10 failures of the same issue_type, it shall group them under a single heading with a count, showing only the first 5 paths. Use `--all` or `--verbose` to see complete list.

**Proposed requirement (R-2.3.8):** For scope-level issues where different drives have independent scopes (507 quota, 403 permissions), the `issues` command shall sub-group by scope. A 507 on the user's own drive and a 507 on a shared folder shortcut are different scopes with different owners, different root causes, and different user actions — they must not be conflated under a single heading.

Example (file-scoped issues — flat grouping by issue_type):
```
FAILURES (100,000 items)

  invalid_filename (95,000 items):
    CON.txt, PRN.doc, AUX.pdf, ~$temp.xlsx, file:name.doc
    ... and 94,995 more (use --verbose to list all)

  path_too_long (5,000 items):
    very/deep/nested/.../file.txt, ...
    ... and 4,995 more

  ACTION REQUIRED: Rename files to comply with OneDrive naming rules.
  Run 'onedrive issues clear --all' after fixing, or exclude via skip_files config.
```

Example (scope-level issues — grouped by scope within issue_type):
```
FAILURES

  quota_exceeded:
    Your OneDrive (50,000 uploads pending):
      Your storage is full (15.0 GB / 15.0 GB).
      ACTION: Free space on OneDrive or upgrade storage plan.

    Shared folder 'Team Docs' (3,000 uploads pending):
      Folder owner's storage is full.
      ACTION: Contact the folder owner to free space.
```

---

## Part 4: Auto-Clearing Stale Failures

Three designs were evaluated. **Design A** (scanner-driven: compare observed paths against sync_failures after walk) was rejected because it couples the scanner to the database — scanner currently has no DB dependency. **Design B** (planner-driven: clear failures for missing files during planning) was rejected because the planner must remain a pure function with no I/O.

### Decision: Design C — Engine-Triggered Cleanup

When the scanner completes and a path is absent from the observed set, or when a `ChangeDelete` event arrives, the engine clears any `sync_failures` entry for that path. This is a dedicated step in the engine's pipeline after observation completes.

- Keeps scanner as a leaf observer, planner as pure, engine as orchestrator
- Handles "file deleted" and "file renamed" (appears as delete + create; the delete triggers cleanup of the old path's failure entry)

Implementation: `SyncStore.ClearStaleUploadFailures(ctx, observedLocalPaths)` called from the engine after local observation. For remote-origin failures (download/delete), clear when success is committed (R-2.10.41).

---

## Part 5: Transient Error Log Levels

### Current behavior

CLAUDE.md logging levels:
- **Warn**: Degraded but recoverable — retries, expired tokens, fallbacks
- **Error**: Terminal failures — request failed after all retries, unrecoverable auth

Currently, every individual retry attempt within `withRetry` logs at WARN (executor.go:313): `"retrying after transient error"`. For 100k files each with 3 executor retries × 5 transport retries, this is up to 1.5M WARN lines.

### Proposed change

**Individual retry attempts** (transport-level, executor-level) should be **DEBUG**, not WARN. The user doesn't need to know about a 404 that resolved on the second try.

**Exhausted retries** (all attempts failed, action is being recorded as failed) should be **WARN** for the first occurrence and **aggregated** thereafter (per Part 3).

**Proposed requirements:**

**R-6.6.8:** Individual HTTP retry attempts for transient errors (5xx, 408, 412, 429, transient 404) shall be logged at DEBUG level. Only the final outcome — success after retry, or failure after exhausting retries — shall be logged at WARN or higher.

**R-6.6.9:** When a transient error resolves within the retry budget (any attempt succeeds), no WARN or higher log shall be emitted. The retry is logged at INFO: `"transient error resolved" operation="upload foo.txt" attempts=3`.

**R-6.6.10:** When a transient error exhausts all retries and is recorded as a failure, a single WARN log shall be emitted with the final error, attempt count, and next retry time.

---

## Part 6: Error Reason & User Action in Warnings and Issues

### Proposed requirement

**R-6.6.11:** Every failure displayed to the user (in log warnings, `issues` output, and `status` output) shall include: (a) the specific error reason in plain language, and (b) the user action required to resolve it, if any.

### Per-error-type reason and action text

> **Note:** Table 3 (User Communication) in the Failure Taxonomy is the canonical version of this table, including scope variants and all transient exhaustion types (`server_error`, `request_timeout`, `transient_conflict`, `transient_not_found`, `resource_locked`). This table preserves the original analysis.

| Error Type | Reason Text | User Action Text |
|-----------|-------------|-----------------|
| `invalid_filename` | "File name '{name}' contains characters not allowed on OneDrive ({chars})" or "File name '{name}' is reserved on OneDrive" | "Rename the file to remove invalid characters, or add it to skip_files in config" |
| `path_too_long` | "Path exceeds OneDrive's 400-character limit ({len} characters)" | "Shorten the directory structure or file name" |
| `file_too_large` | "File exceeds OneDrive's 250 GB size limit ({size})" | "Exclude via skip_files in config" |
| `permission_denied` | "OneDrive folder '{boundary}' is read-only (shared without write access)" | "Request write access from the folder owner, or accept read-only sync" |
| `local_permission_denied` | "Cannot read file (local permission denied)" | "Fix file permissions: chmod/chown the file or its parent directory" |
| `quota_exceeded` (own drive) | "Your OneDrive storage is full ({used}/{total})" | "Free space on OneDrive (delete files, empty recycle bin) or upgrade storage plan" |
| `quota_exceeded` (shared folder) | "Shared folder '{shortcut}' owner's storage is full" | "Contact the folder owner to free space or upgrade their storage plan" |
| `disk_full` (critical) | "Local disk space below minimum ({available} free, {min_free_space} required)" | "Free local disk space. Downloads will resume automatically." |
| `file_too_large_for_space` (per-file) | "Insufficient space for file: need {size}, available {available}" | "Free local disk space or increase min_free_space to 0 to disable reservation" |
| `case_collision` | "Files '{name1}' and '{name2}' differ only in case — OneDrive is case-insensitive" | "Rename one of the files to avoid the collision" |
| `rate_limited` | "OneDrive API rate limit reached (retry in {seconds}s)" | "No action needed — will auto-resolve. Reduce transfer_workers to lower request rate" |
| `service_outage` | "Microsoft Graph API appears unavailable (HTTP {code})" | "No action needed — will auto-resolve when service recovers" |
| `auth_expired` | "Authentication has expired" | "Run 'onedrive login' to re-authenticate" |

---

## Part 7: Retry Architecture — End-to-End Redesign

### 7.1: Why the Current Architecture Is Wrong

The system has three independent retry loops that nest and compound:

**Layer 1 — Transport** (`graph/client.go doRetry`): 5 HTTP attempts with 1-60s exponential backoff. Blocks the worker goroutine during backoff sleep. Handles 429 (Retry-After), 5xx, 408, 412, 404.

**Layer 2 — Executor** (`sync/executor.go withRetry`): 3 action attempts, each triggering a full transport retry cycle. Blocks the worker goroutine. Worst case: 5 transport × 4 executor = 20 HTTP attempts, ~5 minutes blocked per action.

**Layer 3 — Reconciler** (`sync/reconciler.go`): Infinite attempts with 30s-1h exponential backoff. Timer-driven, non-blocking. Re-injects failed items from `sync_failures` database.

**Problems, assessed against Eng Philosophy:**

| Problem | Eng Philosophy Violation |
|---------|------------------------|
| Nested blocking: worker blocked ~5 min per failing action (5 transport × 4 executor retries) | *"Architecture should be extremely robust"* — blocking all 8 workers for minutes is not robust |
| Error classified independently at three layers (graph `isRetryable`, executor `classifyError`, engine `processWorkerResult`) | *"Functions do one thing"* — classification is one responsibility, fragmented across three components |
| No scope recognition: 100k items each independently retry when service is down | *"Prefer large, long-term solutions"* — individual retry for a shared-cause failure is a band-aid |
| Transport retry in sync path: 5 HTTP retries per attempt, when a non-blocking scheduler exists | *"Never settle for 'good enough for now'"* — transport retry was inherited from CLI use case, never redesigned for sync |
| Executor `withRetry` wraps classification AND retry AND sleep in one function | *"Functions do one thing"* — three responsibilities in one loop |
| After refactoring, old architecture traces remain (executor retry loop, transport retry in sync path) | *"No code is sacred... ensure after refactoring the code doesn't show any signs of the old architecture"* |
| 100k files during outage: 100k × 20 = 2M wasted HTTP requests before any scope recognition | *"Architecture should be extremely robust and full of defensive coding practices"* — the opposite of defensive |

**The fundamental question:** Why retry at multiple blocking layers when a single non-blocking scheduler can manage all retry timing, with scope awareness to stop wasting resources on failures that share a root cause?

### 7.2: The Redesigned Architecture

**Principle:** One classification point. One scheduling point. No blocking retries in the sync path. Every real action attempt does useful work — no synthetic probes.

```
[Scanner] → ScanResult{Events, Skipped}
    │
[Engine]
    ├─ recordSkippedItems(Skipped) → sync_failures (actionable, no retry)
    ├─ clearResolvedFailures()
    │
    ├─ [Change Buffer] ← Events
    ├─ [Planner] → ActionPlan
    │
    ├─ [Tracker] ← actions from ActionPlan
    │     ├─ dependency resolution (existing, unchanged)
    │     ├─ scope gate: if scope blocked → held queue
    │     ├─ trial dispatch: when trial timer fires → release ONE held action
    │     └─ Ready() → workers
    │
    ├─ [Workers] (N goroutines)
    │     ├─ pull from Tracker.Ready()
    │     ├─ execute via Executor → Graph client (single attempt, no retry)
    │     └─ send WorkerResult to Engine
    │
    ├─ classifyResult(WorkerResult)  ← SINGLE classification point
    │     ├─ success → commitOutcome, Tracker.Complete(id)
    │     ├─ transient → Tracker.Complete(id), record in sync_failures with next_retry_at, check scope
    │     │                └─ reconciler re-injects when next_retry_at is due
    │     ├─ actionable → sync_failures (no retry), then check scope escalation
    │     ├─ scopeSignal → set ScopeBlock (429 immediate, 507/5xx pattern-based)
    │     └─ skip/fatal → record, Tracker.Complete(id)
    │
    └─ [Reconciler]
          ├─ startup: reset stuck items (downloading → pending_download, etc.)
          ├─ timer: re-inject from sync_failures where next_retry_at <= now
          └─ re-injected items enter normal pipeline (buffer → planner → tracker)
```

**What changes from current architecture:**

| Component | Current | Redesigned | Eng Philosophy Justification |
|-----------|---------|------------|------------------------------|
| Graph client (sync) | 5 retries, blocking sleep, `waitForThrottle` | 0 retries, single HTTP attempt, return raw result | *"No code is sacred"* — graph client serves two callers, shouldn't impose CLI retry on sync |
| Graph client (CLI) | 5 retries, blocking sleep | **Unchanged** — CLI `ls`/`get`/`put` need standalone retry (no tracker) | CLI is a different use case with different constraints |
| Executor `withRetry` | 3-attempt retry loop with `sleepFunc` | **Deleted entirely** — executor becomes thin action dispatch | *"Functions do one thing"* — execute, don't schedule |
| Executor `classifyError` | Error classification in executor | **Moved to engine** `classifyResult()` | Single classification point eliminates contradictions between layers |
| `retry.Action` policy | Used by executor withRetry | **Deleted** — reconciler handles all retry timing via sync_failures | No executor retry → no executor retry policy |
| `retry.SyncTransport` | N/A | **Created then deleted** — sync callers use raw `http.DefaultTransport` directly | No named policy constant needed; sync gets raw result without transport blocking |
| Tracker | Dependency dispatch only | **Extended**: scope gating and dispatch gates. No retry logic — retry handled by sync_failures + reconciler. | *"Prefer large, long-term solutions"* — tracker is the scope-aware dispatch gate |
| Reconciler (FailureRetrier) | Primary retry mechanism (30s-1h) | **Restored**: sole retry mechanism. Re-injects from sync_failures via buffer→planner→tracker. Single backoff curve (1s→1h). Crash recovery via `ResetInProgressStates`. | One retry mechanism, not two. |
| Engine result processing | Records failures, handles 403 | **Extended**: classification, scope detection, retry-vs-record decisions | *"Functions do one thing"* — engine orchestrates, everything else executes |

**Eng Philosophy: "App hasn't been launched yet. No backwards compatibility."** The executor retry loop, transport retry in the sync path, and the `retry.Action` policy are deleted. No compatibility shims. No fallback paths. The new architecture replaces the old one completely.

**Eng Philosophy: "Ensure after refactoring the code doesn't show any signs of the old architecture."** `withRetry`, `sleepFunc`, `classifyError`, `classifyStatusCode` are removed from the executor. The executor becomes a stateless action dispatcher. No `errClassRetryable`, no `errClassFatal` — the engine uses its own result classification that includes scope awareness.

### 7.3: Scope-Aware Retry with Trial Actions

**The problem.** When Microsoft is down, 100k items each independently exhaust their retry budget (currently 20 HTTP attempts each = 2M wasted requests). No component recognizes "the service is down for everyone." The system needs scope awareness: detect that a failure affects more than one file, stop wasting resources, and test for recovery efficiently.

**Scope detection.** The engine maintains a `ScopeState` — a simple struct tracking recent error patterns. After processing each `WorkerResult`, the engine calls `updateScope()`:

| Error | Scope Level | Detection Rule |
|-------|-------------|---------------|
| HTTP 429 | Account (all drives) | **Immediate** from a single 429. This is a server signal, not inference — Microsoft explicitly says "stop." Retry-After duration from the response becomes the initial trial interval. Affects all action types on all drives because 429 is per-authenticated-user (all API calls share the same OAuth token and rate limit, regardless of target drive). |
| HTTP 507 (own drive) | Account uploads (own drives only) | **Pattern**: 3 consecutive 507s from different file paths targeting user's own drives within 10 seconds → `quota_exceeded` scope block on uploads to user's own drives. Does NOT block uploads to shared folders (different quota owner). See §7.4.1. |
| HTTP 507 (shared folder) | Per-shortcut uploads | **Pattern**: 3 consecutive 507s targeting the same shared folder shortcut → `quota_exceeded` scope block on uploads to that specific shortcut only. The sharer's quota is independent from the user's. See §7.4.1. |
| HTTP 5xx (500, 502, 503, 504) | Service | **Pattern**: 5 consecutive 5xx from different file paths within 30 seconds → `service_outage` scope block. Affects all drives — Microsoft Graph API is shared infrastructure. |
| HTTP 503 with Retry-After | Service | **Immediate** from a single 503 with Retry-After header (server signal). Retry-After becomes initial trial interval. |
| HTTP 403 | Folder subtree (per source drive) | **Existing** `handle403` + `walkPermissionBoundary` logic. Scoped to the source drive — a 403 on a shared folder does not affect the user's own drive or other shared folders. Unchanged. |
| `os.ErrPermission` on dir | Directory subtree | Parent directory inaccessible → directory-scope block. See Part 8. |

**ScopeState data structure:**

```go
type ScopeBlock struct {
    Key           string        // see scope key format below
    IssueType     string        // "service_outage", "quota_exceeded", "rate_limited"
    ActionFilter  ActionFilter  // which actions this block applies to (e.g., uploads only)
    BlockedAt     time.Time
    TrialInterval time.Duration // current interval between trial actions
    NextTrialAt   time.Time     // when to dispatch the next trial
    TrialCount    int           // consecutive failed trials (for backoff)
}

// Scope key format (in-memory ScopeState — uses stable internal IDs for matching):
//   "service"                               — all actions, all drives
//   "throttle:account"                      — all actions, all drives (429)
//   "quota:own"                             — uploads to user's own drive paths only
//   "quota:shortcut:$remoteDrive:$remoteItem" — uploads to one specific shortcut only
//   "perm:dir:/local/path"                  — all actions under a local directory
//   "perm:remote:$remoteDrive:$boundary"    — writes under a remote permission boundary
//
// When recording to sync_failures, scope keys use human-readable local paths:
//   "quota:shortcut:Team Docs"              — for display in `issues` command
//   "perm:remote:Shared/Marketing"          — for display in `issues` command
// Translation from internal IDs to local paths happens at recording time.
```

**When a scope block is set.** The tracker moves all matching actions from its ready queue to a per-scope **held queue**. No new actions for the blocked scope are dispatched. Workers that were already executing in-flight actions complete naturally — their results route through the standard path and end up held if the scope is still blocked.

**Trial actions — not probes.** When a scope block is active, the tracker periodically releases exactly ONE action from the held queue as a **trial**. This is a real sync action — an actual upload, download, or delete — not a synthetic health check.

*Eng Philosophy: "Never settle for 'good enough for now'"* — A synthetic probe (`GET /me`) might succeed while uploads are still broken (different endpoint, different server, different quota check). A real action is the definitive test. If it succeeds, you've accomplished real work AND proven the scope is clear.

*Eng Philosophy: "Architecture should be extremely robust"* — No separate probe endpoint to configure or maintain. No false positives from endpoint mismatches. The trial IS the recovery test.

**Trial outcomes:**

- **Trial succeeds:** Engine clears the scope block. Tracker releases all held actions immediately. The trial action's outcome is committed normally — its upload/download/delete completed, moving sync forward.
- **Trial fails with same scope error (e.g., still 507):** Re-hold the action. Double `TrialInterval` (exponential backoff). The failed attempt counts toward the action's individual retry budget — it was a legitimate retry, not wasted work.
- **Trial fails with a different error (e.g., 507 trial gets a 404):** Process the action's error normally (maybe it's a per-file issue). The scope block remains because the trial didn't test the right condition. Schedule another trial from a different held action.

**Trial timing:**

| Scope | First Trial After | Backoff | Max Interval |
|-------|-------------------|---------|-------------|
| `rate_limited` (429) | Retry-After duration from server response | 2× | 10 min |
| `quota_exceeded` (507) | 5 minutes | 2× | 1 hour |
| `service_outage` (5xx) | 60 seconds | 2× | 10 minutes |
| `service_outage` (503 w/ Retry-After) | Retry-After duration | 2× | 10 minutes |

**Race condition: bounded by worker count.** When the engine detects a scope pattern and sets a block, at most `transfer_workers` (default 8) actions may already be dispatched to workers. These in-flight actions complete naturally:

1. Actions 1-3 fail with 500. Engine classifies each as transient, re-queues via tracker.
2. After action 3, engine detects scope pattern → sets service-scope block.
3. Actions 4-8 were already dispatched. They fail, return results, get re-queued.
4. Tracker sees scope block → holds re-queued actions 1-8.
5. No new actions dispatched. Workers idle.
6. Trial timer fires → one held action dispatched.

**Bounded waste: 8 requests** (equal to worker count). Acceptable. No locking needed during dispatch. The alternative — locking dispatch while processing results — would serialize the pipeline and create a bottleneck worse than 8 wasted requests.

### 7.3.1: Scope Detection Window — Implementation Detail

**Data structure.** Each scope key has a sliding time window of recent results:

```go
type ScopeWindow struct {
    entries []windowEntry   // ring buffer, capped at ~20 entries
}

type windowEntry struct {
    timestamp time.Time
    path      string
    success   bool
}
```

**Update rule.** On each `WorkerResult` classified as transient or scope-signal for this scope:

1. Append `{now, path, success}` to the window.
2. Prune entries older than the time window (10s for 507, 30s for 5xx).
3. Evaluate: walk entries forward. A success from any path **resets** the unique-path failure set. A failure from a new path adds to the set. If the set reaches the threshold (3 for 507, 5 for 5xx), trigger scope block.

**Why success resets.** With 8 concurrent workers, results interleave. If even one worker succeeds on a different path, the scope is not fully blocked — some subset of requests is still working. Resetting avoids false scope blocks from interleaved results.

**Concurrent worker interleaving — worst case.** 8 workers dispatch at t=0. Server goes down at t=50ms. Worker 1 returns success (request completed before outage). Workers 2-8 return 502. The success arrives mid-stream and resets the counter. Workers 2-8's results rebuild the counter to 7. If threshold is 5, scope blocks after result 7. Total waste: 8 requests instead of 5. Acceptable — the trial mechanism limits subsequent waste.

**"Different paths" prevents false scope escalation.** If one broken file fails 3 times (transient retry), that's a per-file issue, not a scope issue. The unique-path set ensures scope detection requires N different files failing, proving the problem is scope-level.

**429/503-with-Retry-After bypass pattern detection.** These are server signals — a single response is sufficient. No sliding window needed. `classifyResult()` returns `resultScopeSignal` directly, skipping window-based detection.

### 7.3.2: Scope Lifecycle — Queryable vs Trial-Only

Scope blocks fall into two categories with fundamentally different lifecycles:

**Queryable scopes (permissions).** Can be tested by querying an API endpoint without side effects. The existing `recheckPermissions()` + `permissionCache` pattern handles these: reset the cache at the start of each sync pass, re-query each `permission_denied` entry via `ListItemPermissions`, repopulate the cache with current state, clear entries where permissions have been restored. This per-pass recheck is correct because the Graph API provides a definitive answer. `ScopeState` does NOT manage permission scopes — they stay in `permissionCache`.

**Trial-only scopes (quota, service, throttle, disk).** Cannot be tested by a read-only query — the only way to know if a quota block has cleared is to try a real upload. These persist in `ScopeState` until trial success or process restart. They are NEVER reset per sync pass. `ScopeState` has no `reset()` method. Trial-only scopes use the tracker's held queue + trial timer. On process restart, trial-only scope state is lost (§12.11) — the system re-discovers by burning 3-5 requests, which is cheaper than persisting scope state.

Do not merge these mechanisms. They share the word "scope" but have different lifecycles, different data sources, and different invalidation strategies.

### 7.4: 429 Throttle Scope

**Can we determine the throttle scope from the 429 response?**

Partially. Microsoft doesn't explicitly say "per-user limit" vs "per-tenant limit" in the 429 response. However:

1. **`RateLimit-*` headers (beta):** Microsoft returns `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset` when an app consumes ≥80% of its per-app 1-minute resource unit limit. These only cover one specific limit. If throttled by a different limit (per-user, per-tenant), no `RateLimit` headers are sent.
2. **`Retry-After` value heuristic:** Short (1-10s) typically per-user. Long (30-120s) may indicate broader throttling. This is NOT documented and should not be relied upon.
3. **Contextual inference:** If only one drive is throttled, likely per-user. If all drives across an account, likely per-tenant. Only determinable after the fact.

**Decision:** Treat all 429s as account-scoped. Our app syncs for a single user. Microsoft's per-user limit (3,000 req/5min) is the most likely trigger. Single Retry-After deadline shared across all drives for the same account.

**Proactive rate reduction:** Parse `RateLimit-Remaining` headers when present. When remaining quota falls below 20%, reduce request rate before hitting 429. This is an optimization layered on top, not part of the retry architecture.

**Component assignment (deferred):** The graph client parses RateLimit-Remaining from response headers and exposes it on GraphError. The engine reads it after each WorkerResult and, when remaining quota falls below 20%, reduces dispatch rate (e.g., by throttling the tracker's ready channel). This prevents 429s proactively rather than reacting to them. Implementation is deferred to a post-launch optimization pass — the scope-based retry architecture handles 429s reactively as the primary mechanism. R-6.8.5 status remains [planned].

### 7.4.1: Shared Drives and Scope Boundaries

A single engine handles both the user's primary drive AND shortcuts to shared folders. Shortcuts are observed using the same graph client but target a different drive (the sharer's). This has critical implications for scope detection — a single engine's actions target multiple quota owners and permission domains.

**Architecture context.** The orchestrator runs one engine per configured drive. Each engine:
- Has its own graph client, SQLite DB, tracker, and workers
- Observes its primary delta AND spawns shortcut observations for shared folders
- The shortcut observations use the primary engine's graph client but scope API calls to `RemoteDriveID`
- Actions in the engine's action plan may target the user's own drive OR a shortcut's source drive

**Each engine action knows its target drive.** The planner already tracks which drive and parent an action targets (for path reconstruction and API calls). The scope system uses this to determine which scope a failing action belongs to. This target drive info flows through the full pipeline: `Action` (planner) → `TrackedAction` (tracker, for scope matching) → `WorkerResult` (worker, for engine classification). No component needs to look up drive info — it's carried by the data.

**Scope implications per error type:**

| Error | Same engine? | Same quota? | Same rate limit? | Same permissions? |
|-------|-------------|-------------|------------------|-------------------|
| 507 on own drive | Yes | Own drives share quota | N/A | N/A |
| 507 on shortcut | Yes | **No** — sharer's quota is independent | N/A | N/A |
| 429 on any request | Yes | N/A | **Yes** — all API calls use same OAuth token | N/A |
| 403 on own drive | Yes | N/A | N/A | Own drive permissions |
| 403 on shortcut | Yes | N/A | N/A | **No** — sharer's permissions, independent per shortcut |
| 5xx on any request | Yes | N/A | N/A | N/A — service-level, affects all |

**507 (quota) requires target-drive-aware scoping.** When a 507 occurs:
1. Engine checks the failing action's target: is it the user's own drive or a shortcut?
2. **Own drive:** scope key = `"quota:own"` — blocks uploads to all own-drive paths. Does NOT block uploads to any shortcut.
3. **Shortcut:** scope key = `"quota:shortcut:$remoteDrive:$remoteItem"` — blocks uploads to that specific shortcut only. Does NOT block uploads to own drive or other shortcuts.

**403 (permissions) is already shortcut-aware.** The existing `handle403` + `walkPermissionBoundary` identifies the read-only boundary on the specific source drive. A 403 on shared folder A does not affect shared folder B or the user's own files.

**429 (rate limit) is correctly account-scoped.** All API calls — whether targeting the user's own drive or a shared folder — use the same OAuth token and count against the same per-user rate limit (3,000 req/5min). A 429 on any request means all requests should stop, regardless of target drive.

**Cross-engine coordination is NOT needed.** Each engine discovers scope events independently:
- If Engine A (personal drive) hits 429, Engine B (SharePoint drive) will discover it on its next request (same token, same rate limit). Waste: one request per engine. Acceptable.
- If Engine A hits 507, Engine B's uploads to its own paths will also fail with 507 (same account quota). Engine B discovers independently. Waste: a few requests per engine.
- The exception: Engine A's shortcut to shared folder S will NOT get 507 from Engine A's quota (different quota owner). The scope system correctly blocks only own-drive uploads, not shortcut uploads.

**Tracker scope matching.** When the tracker checks whether an action is blocked by a scope, it must match the action's target drive against the scope key:

```go
func (s *ScopeState) IsBlocked(action Action) string {
    // Service-level blocks affect everything
    if _, ok := s.blocks["service"]; ok { return "service" }

    // Account-level throttle affects everything
    if _, ok := s.blocks["throttle:account"]; ok { return "throttle:account" }

    // Quota blocks: check if action targets own drive or a specific shortcut
    if action.IsUpload() {
        if action.TargetsOwnDrive() {
            if _, ok := s.blocks["quota:own"]; ok { return "quota:own" }
        } else {
            key := fmt.Sprintf("quota:shortcut:%s:%s", action.RemoteDrive, action.RemoteItem)
            if _, ok := s.blocks[key]; ok { return key }
        }
    }

    // Permission blocks: check folder subtree
    // ... existing boundary matching logic ...

    return "" // not blocked
}
```

### 7.5: Unified Backoff Schedule

One non-compounding backoff schedule for all transient errors. No nested loops. A single mechanism: sync_failures + reconciler.

| Attempt | Backoff (next_retry_at) | Worker Blocks? | Mechanism |
|---------|------------------------|----------------|-----------|
| 1st attempt | — | HTTP RTT only (~100-500ms) | Worker → Graph client (single attempt) |
| Failure recorded | 1s | No | sync_failures with next_retry_at = now + 1s |
| 2nd attempt | — | HTTP RTT only | Reconciler re-injects → buffer → planner → tracker → worker |
| Failure recorded | 2s | No | sync_failures with next_retry_at = now + 2s |
| 3rd attempt | — | HTTP RTT only | Reconciler re-injects |
| Failure recorded | 4s | No | sync_failures with next_retry_at = now + 4s |
| 4th attempt | — | HTTP RTT only | Reconciler re-injects |
| Failure recorded | 8s | No | sync_failures with next_retry_at = now + 8s |
| ... | doubles | No | Up to 1 hour max |

All retry is via sync_failures + reconciler. There is no in-memory retry budget, no tracker re-queue, no escalation between mechanisms. The backoff curve is: 1s, 2s, 4s, 8s, 16s, 32s, 64s, 128s, 256s, 512s, 1024s, 2048s, 3600s (capped at 1h), with ±25% jitter.

**Comparison with current architecture:**

| Metric | Current (nested) | Redesigned (unified) |
|--------|-----------------|---------------------|
| HTTP attempts before persistent record | 20 (5 transport × 4 executor) | 1 |
| Time to first persistent record | ~5 minutes | Immediate (after first failure) |
| Worker blocked per action | ~5 minutes | ~500ms (HTTP RTT only) |
| Worker blocked during outage (8 workers) | All 8 for ~5 min each | All 8 for ~500ms each |
| Wasted requests for 100k files during outage | 2,000,000 | 3-5 (scope detection) + periodic trials |

**If scope detection triggers** (e.g., after 3 consecutive failures from different files), remaining actions never attempt. They sit in the held queue until a trial action succeeds. Total wasted work during a 30-minute outage: 3 scope detection requests + ~15 trial actions = ~18 requests. Compare to current: 2M requests.

**Crash recovery.** All retry state is in sync_failures (SQLite), so it survives crashes. On restart, the reconciler resets stuck items, the delta observer re-observes changes, and the idempotent planner re-creates actions. No in-memory retry state to lose.

### 7.6: Component Details

**TrackedAction extensions:**

```go
type TrackedAction struct {
    // Existing fields (unchanged)
    ID          int
    Action      Action      // carries TargetDriveID, RemoteItem, Path — used by scope matching
    depsLeft    atomic.Int32
    dependents  []*TrackedAction

    // New fields for scope gating
    IsTrial       bool    // true when released as a scope trial action
    TrialScopeKey string  // scope key this trial is testing (empty if not a trial)
}
```

The tracker has no retry logic — no NotBefore, no Attempt, no MaxAttempts. Retry is handled entirely by sync_failures + reconciler. The tracker's role is dependency resolution, scope-aware dispatch gating, and trial action dispatch.

The `Action` field already contains target drive information needed for scope matching. The tracker's `ScopeState.IsBlocked(action)` (§7.4.1) reads `action.TargetsOwnDrive()` and `action.RemoteDrive`/`action.RemoteItem` to match against scope keys. No additional fields needed on `TrackedAction` — the scope context flows from the action itself.

**Tracker dispatch changes (pseudocode):**

**Integration with DepTracker.** The scope gate is inserted AFTER dependency resolution, not as a top-level entry point. In the existing codebase, `DepTracker.dispatch()` is called when `depsLeft` reaches 0 (all dependencies satisfied). The scope gate wraps this dispatch point:

```go
// dispatch is called when all dependencies are satisfied (depsLeft == 0).
// This is the ONLY dispatch point — called from Add() (no deps) and
// Complete() (dependent's last dep satisfied).
func (dt *DepTracker) dispatch(ta *TrackedAction) {
    // Single gate: Scope block. If scope is blocked, hold the action.
    // When scope clears, releaseScope() re-calls dispatch() for each held action.
    if scopeKey := dt.scopeState.BlockedScope(ta.Action); scopeKey != "" {
        dt.held[scopeKey] = append(dt.held[scopeKey], ta)
        return
    }
    // Gate passed: send to workers.
    dt.ready <- ta
}

// Called by scope trial timer
func (dt *DepTracker) dispatchTrial(scopeKey string) {
    items := dt.held[scopeKey]
    if len(items) == 0 { return }
    trial := items[0]
    trial.IsTrial = true
    trial.TrialScopeKey = scopeKey
    dt.held[scopeKey] = items[1:]
    dt.ready <- trial
}

// Called when engine clears a scope block
func (dt *DepTracker) releaseScope(scopeKey string) {
    for _, ta := range dt.held[scopeKey] {
        ta.IsTrial = false
        ta.TrialScopeKey = ""
        dt.dispatch(ta)  // re-evaluate: check other scopes
    }
    delete(dt.held, scopeKey)
}
```

A timer per scope block fires trial dispatches at the configured interval. There is no delayed queue — retry timing is managed by sync_failures `next_retry_at` + reconciler.

**WorkerResult struct (carries target drive context):**

```go
type WorkerResult struct {
    ActionID    int
    Action      Action        // the original action (carries TargetDriveID, RemoteItem, Path)
    Success     bool
    HTTPStatus  int
    Err         error
    RetryAfter  time.Duration // parsed from Retry-After header (429, 503)

    // Target drive context — populated by worker from the action
    // Engine uses this to determine which scope a failure belongs to.
    // Actions targeting the user's own drive: TargetDriveID == engine's primary drive.
    // Actions targeting a shortcut: TargetDriveID == sharer's RemoteDriveID.
    TargetDriveID string       // which drive this action targeted
    ShortcutKey   string       // "remoteDrive:remoteItem" if shortcut, empty if own drive
}
```

The worker populates `TargetDriveID` and `ShortcutKey` from the action before sending the result. This is the **only** way the engine knows which quota owner and permission domain a failure belongs to. Without it, the engine cannot distinguish "your drive is full" from "shared folder owner's drive is full."

**Engine classification (single point, target-drive-aware):**

```go
func (e *Engine) classifyResult(r WorkerResult) resultClass {
    if r.Success {
        return resultSuccess
    }
    switch {
    case r.HTTPStatus == 429:
        return resultScopeSignal   // immediate account-scope block (all drives, same token)
    case r.HTTPStatus == 507:
        return resultActionable    // quota_exceeded (scope key depends on target drive — see updateScope)
    case r.HTTPStatus == 403:
        return resultActionable    // permission_denied (scope key depends on target drive — see handle403)
    case r.HTTPStatus == 401:
        return resultFatal         // auth expired, unrecoverable
    case r.HTTPStatus == 400 && isOutagePattern(r.Err):
        return resultTransient     // known outage pattern (e.g., "ObjectHandle is Invalid") — feeds scope detection
    case r.HTTPStatus >= 500:
        return resultTransient     // server error, retry via tracker
    case r.HTTPStatus == 404 || r.HTTPStatus == 408 || r.HTTPStatus == 412 || r.HTTPStatus == 423:
        return resultTransient     // known transient HTTP codes (423 = SharePoint lock, self-resolving)
    case errors.Is(r.Err, os.ErrPermission):
        return resultActionable    // local permission denied
    case errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded):
        return resultShutdown   // graceful drain: discard action, don't record failure
    default:
        // NOTE: The default case should log at WARN level before returning,
        // so unrecognized errors are surfaced for investigation rather than
        // silently skipped. (Issue 4: doc-only fix)
        return resultSkip          // unknown error, don't retry
    }
}

// After classification, engine routes scope actions using target drive context:
func (e *Engine) updateScope(r WorkerResult, class resultClass) {
    switch {
    case r.HTTPStatus == 429:
        // Account-wide: same OAuth token, same rate limit for all drives
        e.scopeState.SetBlock("throttle:account", ...)
    case r.HTTPStatus == 507 && r.ShortcutKey == "":
        // Own drive: user's quota is shared across all non-shortcut drives
        e.scopeState.AddToWindow("quota:own", r)
    case r.HTTPStatus == 507 && r.ShortcutKey != "":
        // Shared folder: sharer's quota is independent
        e.scopeState.AddToWindow("quota:shortcut:"+r.ShortcutKey, r)
    case r.HTTPStatus == 403 && r.ShortcutKey != "":
        // Shared folder permissions: independent per shortcut
        e.handle403ForShortcut(r)
    case r.HTTPStatus == 403:
        // Own drive permissions: existing handle403 + walkPermissionBoundary
        e.handle403(r)
    case r.HTTPStatus >= 500:
        // Service-level: Graph API shared infrastructure, affects all drives
        e.scopeState.AddToWindow("service", r)
    case r.HTTPStatus == 400 && isOutagePattern(r.Err):
        // Known outage pattern (e.g., "ObjectHandle is Invalid") — route to service scope
        e.scopeState.AddToWindow("service", r)
    }
}
```

```go
// isOutagePattern distinguishes transient service outages from permanent
// phantom drive 400s (R-6.7.11). Phantom drives fail on one specific drive
// ID and are handled by drive filtering, not scope detection.
func isOutagePattern(err error) bool {
    var ge *graph.GraphError
    if !errors.As(err, &ge) {
        return false
    }
    return ge.InnerError.Code == "invalidRequest" &&
        strings.Contains(ge.Message, "ObjectHandle is Invalid")
}
```

This replaces the three-way classification that currently exists in `graph/errors.go isRetryable()`, `sync/executor.go classifyError()/classifyStatusCode()`, and `sync/engine.go processWorkerResult()`. One function. One place. One set of rules. **Target drive context flows through the entire pipeline: Action → Worker → WorkerResult → classifyResult → updateScope → tracker scope key.**

*Eng Philosophy: "Functions do one thing"* — `classifyResult` classifies. `updateScope` routes to the correct scope using target drive context. The tracker schedules. The worker executes.

**Graph client changes for sync callers:**

The `Client` struct currently has no retry policy field — `doRetry` and `doPreAuthRetry` both hardcode `retry.Transport.MaxAttempts`. This must be parameterized: add a `retryPolicy retry.Policy` field to `Client`, accept it as a constructor parameter (defaulting to `retry.Transport`), and replace all four occurrences of `retry.Transport.MaxAttempts` in `doRetry` (lines 124, 166) and `doPreAuthRetry` (lines 318, 352) with `c.retryPolicy.MaxAttempts`. Sync callers pass `SyncTransport`; CLI callers pass `Transport` (or omit for the default).

```go
var SyncTransport = retry.Policy{MaxAttempts: 0}  // single attempt, no retry
```

With `MaxAttempts: 0`, the `for {}` loop in `doRetry` executes `doOnce()` once (first iteration always runs), then the retry check `attempt < 0` is false, so it returns immediately. One HTTP request, no sleep, no retry. The same applies to `doPreAuthRetry`, which handles upload chunks and download streams — the primary sync I/O path. Both paths must use the parameterized policy.

**`waitForThrottle` remains as defense-in-depth.** `waitForThrottle(ctx)` is called at the start of `doRetry` (line 107), before the retry loop. The primary 429 gate is the tracker's scope block, which prevents dispatch. But during the race window (up to `transfer_workers` requests already dispatched), `waitForThrottle` catches stragglers. This layered approach — tracker prevents new dispatch, graph client gates in-flight requests — ensures zero wasted requests after the Retry-After deadline. Note: `doPreAuthRetry` does NOT call `waitForThrottle` because pre-auth URLs go directly to Azure blob storage, bypassing the Graph API rate-limit domain — this asymmetry is correct and must be preserved.

For `GraphError`, add `RetryAfter time.Duration` field populated from `Retry-After` header on 429 and 503 responses. The engine reads this to set scope block duration.

**401 and token refresh.** `SyncTransport` (0 retries) does NOT disable auth token refresh. When a 401 occurs, the graph client performs a single token refresh and re-attempts the request. This is auth lifecycle, not transient error retry — a 401 means the token expired, and token refresh is expected to succeed. If the refreshed token also receives 401, the graph client returns `ErrUnauthorized` (fatal). The `MaxAttempts: 0` policy means "no retries for transient HTTP errors (5xx, 408, etc.)," not "no auth handling." Auth refresh happens transparently inside `doOnce()`, not in `doRetry`.

For CLI callers (`ls`, `get`, `put`), the graph client continues to use `retry.Transport` (5 retries, blocking). No changes.

### 7.7: What Happens to Existing Named Policies

| Policy | Current Use | After Redesign |
|--------|------------|----------------|
| `Transport` | Graph client HTTP retry (5 attempts, 1-60s) | **Unchanged** — used by CLI callers only |
| `SyncTransport` | Was MaxAttempts: 0, used by sync workers | **Deleted** — sync callers use raw `http.DefaultTransport` directly (no named policy constant needed) |
| `DriveDiscovery` | Drive initialization (3 attempts) | **Unchanged** — not part of sync action path |
| `Action` | Executor `withRetry` (3 attempts) | **Deleted** — executor retry loop removed |
| `Reconcile` | Reconciler persistent retry (infinite, 30s-1h) | **Restored**: Base=1s, Max=1h, 2× multiplier, ±25% jitter. Single retry curve for all sync failures. FailureRetrier is the sole retry mechanism. |
| `WatchLocal` | Local observer retry (infinite, 1-30s) | **Unchanged** — observer-level, not action-level |
| `WatchRemote` | Remote observer retry (infinite, 5s-5min) | **Unchanged** — observer-level, not action-level |

### 7.8: Requirements

**R-6.8.7 (revised):** During sync operations, workers shall never block on retry backoff. When a transient error occurs, the action shall be completed in the tracker and the failure recorded in sync_failures with a `next_retry_at` timestamp. The worker shall immediately pull the next available action. Workers block only for HTTP round-trip time (~100-500ms per attempt), never for backoff sleeps.

**R-6.8.8 (new):** For sync operations, the graph client shall use a zero-retry transport policy (`SyncTransport`, `MaxAttempts: 0`). Each worker dispatch results in exactly one HTTP request. The graph client shall retain its full retry policy (`Transport`, `MaxAttempts: 5`) for standalone CLI operations. The transport policy shall be a constructor parameter.

**R-6.8.9 (new):** The executor shall not contain a retry loop. The executor's responsibility is action dispatch: call the appropriate graph client or filesystem operation and return the result. Error classification and retry scheduling are the engine's responsibility. The `withRetry` function, `classifyError`, `classifyStatusCode`, `sleepFunc`, and `errClass` constants shall be removed from the executor.

**R-2.10.3 (revised):** The system shall detect scope-level failure patterns and block further dispatch for the affected scope:
- HTTP 429: Immediate account-scope block from a single 429 response, duration from `Retry-After` header. Blocks all action types on all drives (rate limit is per-authenticated-user).
- HTTP 507 on own drive: Account-scope upload block after 3 consecutive 507s from different own-drive file paths within 10 seconds. Blocks uploads to user's own drives only. Does NOT block uploads to shared folder shortcuts (independent quota).
- HTTP 507 on shared folder: Per-shortcut upload block after 3 consecutive 507s targeting the same shortcut. Blocks uploads to that specific shortcut only.
- HTTP 5xx: Service-scope block after 5 consecutive 5xx errors from different file paths within 30 seconds. Affects all drives.
- HTTP 503 with Retry-After: Immediate service-scope block, duration from `Retry-After` header.
Scope blocks shall prevent the tracker from dispatching new actions matching the scope's action filter. Actions already in-flight complete normally; their results route through the standard retry path.

**R-2.10.5 (revised):** When a scope block is active, the system shall test for recovery by periodically releasing one real action from the held queue as a trial. The system shall NOT use synthetic probe requests. On trial success: clear the scope block and release all held actions immediately. On trial failure with the same scope error: re-hold the action and extend the trial interval (exponential backoff). The trial action is a real sync operation that accomplishes work on success.

**R-2.10.14 (new):** Trial timing: `rate_limited` first trial at `Retry-After` duration (max 10 min); `quota_exceeded` first trial at 5 minutes (2× backoff, max 1 hour); `service_outage` first trial at 60 seconds (2× backoff, max 10 minutes); `service_outage` with `Retry-After` first trial at `Retry-After` duration (2× backoff, max 10 minutes).

**R-2.10.15 (new):** When a scope block is set, at most `transfer_workers` actions may be in-flight. These complete normally and route through standard retry. This bounded waste (equal to worker count) is accepted as the cost of lock-free dispatch. No locking between result processing and action dispatch.

**R-6.8.10 (new):** When a transient error occurs, the action shall be immediately recorded in `sync_failures` with a `next_retry_at` timestamp. There is no in-memory retry budget — all retry is via sync_failures + reconciler. The reconciler periodically checks for entries where `next_retry_at <= now` and re-injects them into the normal pipeline (buffer → planner → tracker → worker). On success, the sync_failures entry is cleared.

**R-6.8.11 (new):** The unified backoff schedule shall be a single exponential curve via sync_failures + reconciler: 1s, 2s, 4s, 8s, 16s, 32s, 64s, 128s, 256s, 512s, 1024s, 2048s, 3600s (capped at 1 hour), with ±25% jitter. There is no two-phase escalation — one mechanism, one curve. No retry duration shall be multiplied by another retry duration.

**R-6.8.4 (unchanged):** The system shall treat HTTP 429 as account-scoped: when any drive receives 429, all drives under the same account shall respect the `Retry-After` deadline.

**R-6.8.5 (unchanged):** The system shall parse `RateLimit-Remaining` headers (when present) and proactively reduce request rate when remaining quota falls below 20%.

**R-6.8.6 (unchanged):** The system shall extract and honor `Retry-After` headers from both 429 and 503 responses.

---

## Part 8: Local Permission Hierarchical Checking

### Analysis: Does the 403 pattern make sense for local permissions?

The remote 403 handling (`handle403` → `walkPermissionBoundary`) works by:
1. Detecting 403 on a specific file
2. Querying the folder's permissions via Graph API
3. Walking up the hierarchy to find the highest unreadable ancestor
4. Recording the boundary as a `permission_denied` issue
5. Suppressing all writes under that boundary
6. Rechecking each sync pass

**For local permissions, the analog would be:**
1. Detect `os.ErrPermission` on a specific file
2. Check if the parent directory is readable (`os.Open` + `ReadDir`)
3. Walk up to find the highest unreadable ancestor
4. Record the boundary as `local_permission_denied`
5. Suppress all uploads under that boundary
6. Recheck each sync pass

**Does this make sense?** Yes, with caveats:

**Where it works well:**
- A directory owned by root with 100k files: all 100k share the same root cause. Identifying the directory as the boundary means one issue entry instead of 100k.
- The scanner already walks directories — if it can't read a directory, it skips the entire subtree (`filepath.SkipDir`). So the scanner already has implicit boundary detection for unreadable directories.

**Where it doesn't apply:**
- Individual files with restrictive permissions (e.g., `chmod 000 file.txt` but directory is readable): the boundary IS the file itself. No hierarchy to walk.
- Mixed permissions in a directory (some files readable, some not): the boundary isn't a clean subtree.

**Recommendation:** Implement a simplified version:

1. When `os.ErrPermission` occurs during upload, check if the file's parent directory is readable.
2. If the directory is unreadable: record `local_permission_denied` at the directory level, suppress all uploads under it, recheck on next pass.
3. If the directory IS readable but the file isn't: record `local_permission_denied` at the file level (no boundary walk).

This handles the common case (entire directory unreadable) without the complexity of full boundary walking.

**Proposed requirements:**

**R-2.10.12:** When a local file operation fails with permission denied, the system shall check whether the parent directory is accessible. If the directory itself is inaccessible, the system shall record a single `local_permission_denied` issue at the directory level and suppress operations for all items under that directory.

**R-2.10.13:** The system shall recheck `local_permission_denied` directory-level issues at the start of each sync pass, and auto-clear them when the directory becomes accessible.

---

## Part 9: To-Be Journeys

Each journey traces the exact code path in the redesigned architecture from Part 7. No hand-waving. Every step identifies which component acts, what data flows, and what the user sees.

### Journey 1 (TO-BE): Invalid Filename — 100k files

This is an observation-time issue. The retry architecture is not involved — invalid filenames are actionable failures detected before any action is planned.

**Phase 1: Observation (Scanner)**

1. Engine calls `engine.RunOnce()` → `observeLocal()` → `scanner.Scan(ctx, root)`.
2. Scanner walks the filesystem via `filepath.WalkDir`. For each file:
   a. Applies filter cascade: dotfiles, skip_dirs, skip_files, symlinks, vault, marker files.
   b. Calls `isValidOneDriveName(name)` (scanner.go:271).
   c. **Invalid** → appends `SkippedItem{Path: "CON.txt", Reason: "invalid_filename", Detail: "reserved name: CON"}` to `ScanResult.Skipped`.
   d. **Valid** → creates `ChangeEvent` as today, appends to `ScanResult.Events`.
3. Scanner returns `ScanResult{Events: [N valid changes], Skipped: [100k invalid files]}`.
4. Scanner has no DB dependency — it returns data. *Eng Philosophy: "Functions do one thing"* — scanner observes, engine records.

**Phase 2: Issue Recording (Engine)**

5. Engine receives `ScanResult`. Calls `recordSkippedItems(ctx, result.Skipped)`:
   a. Groups skipped items by reason: `{invalid_filename: 95k, path_too_long: 4k, file_too_large: 1k}`.
   b. For each group, calls `store.UpsertActionableFailures(ctx, items)`:
      ```sql
      INSERT INTO sync_failures (path, issue_type, category, error_message, ...)
      VALUES (?, 'invalid_filename', 'actionable', 'reserved name: CON', ...)
      ON CONFLICT(path) DO UPDATE SET updated_at = ?, error_message = ?
      ```
   c. Single SQL transaction per group, batched 500 rows per statement. ~1-2 seconds for 100k rows.
6. Engine calls `store.ClearResolvedActionableFailures(ctx, "invalid_filename", currentSkippedPaths)`:
   ```sql
   DELETE FROM sync_failures
   WHERE issue_type = 'invalid_filename' AND path NOT IN (... current skipped paths ...)
   ```
   This handles auto-cleanup: if a file was previously invalid but the user renamed it, it's no longer in `Skipped`, so its failure entry is deleted.

**Phase 3: Logging (Engine)**

7. Engine aggregates skip counts. For each reason with count > 10, one WARN line:
   ```
   WARN "files skipped" reason="invalid_filename" count=95000 sample=["CON.txt","PRN.doc","AUX.pdf"]
   WARN "files skipped" reason="path_too_long" count=4000 sample=["very/deep/.../file.txt"]
   WARN "files skipped" reason="file_too_large" count=1000 sample=["huge-backup.tar.gz"]
   ```
8. Individual paths logged at DEBUG level only.

**Phase 4: Planning**

9. Only `ScanResult.Events` (valid files) enter the change buffer → planner → `ActionPlan`.
10. Invalid files are absent. No actions planned for them. No worker time will be spent.

**Phase 5: (Removed)**

11. All validation moved to observation layer. Upload validator deleted. Invalid files are filtered by `shouldObserve()` (Stage 1: name + path length, before stat) and `processEntry`/watch handlers (Stage 2: file size, after stat) in the scanner/observer. No separate validation pass on the `ActionPlan`.

**Phase 6: Execution**

12. Workers execute actions for valid files only. All 8 workers are available. Zero worker time wasted on invalid files.

**Phase 7: User Experience**

13. `onedrive sync` console:
    ```
    WARN 95,000 files skipped: invalid OneDrive filename
    WARN 4,000 files skipped: path exceeds 400-character limit
    WARN 1,000 files skipped: file exceeds 250 GB size limit
    Synced 50,000 files (2.3 GB) in 4m 12s
    ```

14. `onedrive issues`:
    ```
    FAILURES (100,000 items)

      invalid_filename (95,000 items):
        CON.txt — reserved name 'CON'. Rename or add to skip_files.
        PRN.doc — reserved name 'PRN'. Rename or add to skip_files.
        ~$temp.xlsx — starts with '~$' (reserved). Rename the file.
        file:name.doc — contains ':' (invalid). Rename the file.
        AUX.pdf — reserved name 'AUX'. Rename or add to skip_files.
        ... and 94,995 more (use --verbose to list all)

      path_too_long (4,000 items):
        very/deep/nested/.../file.txt (423 chars) — exceeds 400-char limit. Shorten path.
        ... and 3,999 more

      file_too_large (1,000 items):
        huge-backup.tar.gz (312 GB) — exceeds 250 GB limit. Exclude via skip_files.
        ... and 999 more
    ```

**Phase 8: Resolution**

15. User renames `CON.txt` to `CON_file.txt`.
16. Next `onedrive sync`:
    a. Scanner: `CON_file.txt` passes `isValidOneDriveName` → `ChangeEvent{ChangeCreate}`.
    b. `CON.txt` is gone → no longer in `ScanResult.Skipped`.
    c. `ClearResolvedActionableFailures` deletes `CON.txt` from `sync_failures`.
    d. Planner creates `ActionUpload` for `CON_file.txt`.
    e. Worker executes upload → success. Baseline updated.
17. `onedrive issues` shows 99,999 remaining. No manual `issues clear` needed.

**Scope:** File-scoped only. No scope escalation. No impact on workers, drives, or accounts. All workers are always available for valid files.

### Journey 2 (TO-BE): OneDrive Full (507) — 100k uploads

This is a scope-aware retry scenario. The first few failures detect a shared cause (quota exhausted), block further uploads, and use trial actions to detect recovery.

**Phase 1: First Uploads Hit 507**

1. Tracker dispatches upload action 1 to worker 1.
2. Worker 1 calls `executor.executeUpload()` → graph client (`SyncTransport`, 0 retries) → single HTTP PUT → 507 Insufficient Storage.
3. Graph client returns `GraphError{StatusCode: 507, Err: ErrServerError}`. No transport retry (MaxAttempts=0).
4. Worker 1 sends `WorkerResult{Path: "file1.txt", HTTPStatus: 507, Success: false, TargetDriveID: "own", ShortcutKey: ""}` to engine. Worker 1 immediately pulls next action from tracker.
5. Simultaneously, workers 2-3 also hit 507 on different files.

**Phase 2: Scope Detection (target-drive-aware)**

6. Engine receives result 1. `classifyResult()` → 507 → `resultActionable` (issue_type: `quota_exceeded`). Engine completes action 1 in tracker, records in sync_failures with `next_retry_at = now + 1s`, kicks reconciler.
7. Engine checks action's target drive: this upload targets the user's own drive (not a shortcut).
8. Engine records in sync_failures: `category='actionable', issue_type='quota_exceeded'`.
9. Engine `updateScope()`: adds to own-drive sliding window. 1 of 3 needed for pattern.
10. Engine receives results 2, 3. Same classification, same target (own drive). Window now has 3 consecutive 507s from different own-drive files within 10 seconds.
11. **Scope block set:** `ScopeBlock{Key: "quota:own", IssueType: "quota_exceeded", ActionFilter: UploadsOnly, NextTrialAt: now+5min}`.
12. Engine tells tracker: block uploads targeting the user's own drive.

**Phase 3: Held Queue (shortcut-aware)**

13. Tracker moves all pending own-drive upload actions to held queue.
14. **Uploads to shared folders (shortcuts) continue.** Shortcuts have independent quotas (the sharer's storage). A 507 on the user's own drive does not affect shared folder uploads.
15. Workers 4-8 may have been dispatched before the block (race, bounded by 8). Own-drive uploads fail with 507, results come back, tracker holds them. Any in-flight shortcut uploads proceed normally.
16. **Downloads, deletes, and moves continue.** The scope block affects own-drive uploads only.
17. All 8 workers are available for shortcut uploads, downloads, deletes, and moves.

**Phase 3a: Shared Folder 507 (separate scenario)**

If a 507 occurs on a shared folder shortcut instead:
- Engine checks action target: this upload targets shortcut `$remoteDrive:$remoteItem`.
- Scope key: `"quota:shortcut:$remoteDrive:$remoteItem"` — blocks uploads to THIS shortcut only.
- Uploads to the user's own drive and other shortcuts continue unaffected.
- Trial actions test the specific shortcut's quota, not the user's.

**Phase 4: Trial Actions**

18. 5 minutes pass. Tracker's trial timer fires for scope `"quota:own"`.
19. Tracker releases ONE own-drive upload from held queue, marked `IsTrial: true`.
20. Worker picks up trial action, executes upload → 507 (still full).
21. Engine receives result. Trial failed with same scope error → re-hold the action. `TrialInterval` doubles to 10 minutes. `NextTrialAt = now + 10min`.
22. Repeat: next trial at 10 min, then 20 min, then 40 min, capping at 1 hour.

**Phase 5: Recovery**

23. User frees space on OneDrive (deletes files, empties recycle bin).
24. Trial timer fires. Tracker releases one own-drive upload. Worker executes → **200 success**.
25. Engine receives trial result: success → **clears scope block** `"quota:own"`.
26. Trial action's outcome committed normally (file uploaded, baseline updated).
27. Tracker `releaseScope("quota:own")` → all held own-drive uploads dispatched, 8 at a time.
28. Workers process the backlog. If some uploads fail again (partial space freed), scope re-blocks with the same mechanism.

**Phase 6: User Experience**

27. `onedrive sync` during block:
    ```
    WARN OneDrive storage full — uploads paused (100,000 pending). Free space to resume.
    INFO Downloads continuing normally.
    ```
28. `onedrive issues`:
    ```
    quota_exceeded (100,000 uploads pending):
      OneDrive storage full (15.0 GB / 15.0 GB).
      ACTION: Free space on OneDrive or upgrade storage plan.
      Next retry: trial upload in 5m.
    ```
29. `onedrive status` (future): shows "uploads paused (quota)" per drive.

**Scope:** Quota-owner-level. Own-drive uploads are blocked when user's quota is full; shared folder uploads are independent (sharer's quota). A 507 on shortcut S blocks only uploads to S, not other shortcuts or own-drive. Downloads, deletes, and moves continue regardless. Workers remain available for all non-blocked actions. Trial actions use real uploads targeting the correct quota owner.

### Journey 3 (TO-BE): 429 Rate Limiting — 100k files

429 is a server signal — Microsoft explicitly says "stop for N seconds." Unlike 507 or 5xx, scope detection is immediate from a single response.

**Phase 1: First 429**

1. Worker 1 calls graph API → 429, `Retry-After: 30`.
2. Graph client (`SyncTransport`, 0 retries) parses `Retry-After` header, populates `GraphError{StatusCode: 429, RetryAfter: 30s}`. Returns immediately. No sleep.
3. Worker 1 sends `WorkerResult{HTTPStatus: 429, RetryAfter: 30s}` to engine.

**Phase 2: Immediate Scope Block**

4. Engine `classifyResult()` → 429 → `resultScopeSignal`. A single 429 is sufficient — this is the server saying "stop," not a pattern to infer. Target drive in the `WorkerResult` is irrelevant for 429 — the scope is account-wide.
5. Engine sets `ScopeBlock{Key: "throttle:account", IssueType: "rate_limited", NextTrialAt: now+30s}`.
6. Tracker holds ALL actions — uploads, downloads, deletes on all drives including shortcuts. 429 is per-authenticated-user: all API calls (whether targeting the user's own drive or a shared folder via shortcut) use the same OAuth token and share the same rate limit. A 429 on a shortcut request means the user's own-drive requests are also throttled, and vice versa.

**Phase 3: Workers Idle**

7. Workers 2-8 may have been in-flight (race, bounded by 8). If they also get 429, same handling. If they succeed (request hit before throttle), success committed normally.
8. No new actions dispatched. Workers idle on `tracker.Ready()`.
9. Unlike 507 (which only blocks uploads), 429 blocks everything for this account. No useful API work is possible. Workers that serve other accounts (in multi-account setups) continue normally.

**Phase 4: Trial After Retry-After**

10. 30 seconds pass. Trial timer fires.
11. Tracker releases ONE action from held queue as trial.
12. Worker executes → **200 success**. Rate limit has cleared.
13. Engine clears scope block. Trial action committed (real work done).
14. Tracker releases all held actions, 8 at a time.
15. Sync resumes at full throughput.

**Phase 5: User Experience**

16. Logging:
    ```
    INFO API rate limit reached, pausing requests for 30s (account: user@example.com)
    INFO Rate limit cleared, resuming sync.
    ```
17. No WARN. 429 is transient and self-resolving. Individual retry attempts logged at DEBUG.
18. `onedrive issues` shows nothing (429 is transient, not actionable). Individual actions that failed with 429 are recorded in sync_failures with `next_retry_at` for reconciler-based retry, but are cleared on success.

**Scope:** Account-level. All drives (own drive AND all shortcuts), all action types. Duration bounded by Retry-After from server. Workers idle (correct — API rejects all requests during throttle, regardless of target drive). No wasted requests. This is the ONLY scope type where shortcuts and own-drive actions are blocked identically — because the bottleneck is the OAuth token, not the target drive's resources.

### Journey 4 (TO-BE): Local Permission Denied — 100k files

This is an actionable failure with directory-level scope detection per Part 8.

**Phase 1: First Upload Fails**

1. Worker 1 calls `executor.executeUpload()` → `os.Open(localPath)` → `os.ErrPermission`.
2. Worker sends `WorkerResult{Err: os.ErrPermission, Success: false}` to engine.

**Phase 2: Hierarchical Check**

3. Engine `classifyResult()` → `errors.Is(err, os.ErrPermission)` → `resultActionable`.
4. Engine checks parent directory: `os.Open("/data/protected/")` → `os.ErrPermission`.
5. Parent directory is unreadable. Engine records ONE `local_permission_denied` at directory level:
   ```sql
   INSERT INTO sync_failures (path, issue_type, category, error_message)
   VALUES ('/data/protected/', 'local_permission_denied', 'actionable',
           'Directory /data/protected/ is not readable')
   ```
6. Engine sets directory-scope block: `ScopeBlock{Key: "dir:/data/protected/", IssueType: "local_permission_denied"}`.
7. Tracker holds all actions whose path starts with `/data/protected/`.

**Phase 3: No Retry**

8. Actionable issues are not auto-retried. No trial actions for `local_permission_denied` — this requires human action (chmod/chown), not a server condition that self-heals.
9. Workers are free for actions outside `/data/protected/`.

**Phase 4: If Individual Files (not directory)**

10. Alternative scenario: parent directory IS readable but specific files are `chmod 000`.
11. Engine records file-level `local_permission_denied` for each failing file. No directory-scope block (the directory is fine, individual files are restricted).
12. Aggregated logging: 1 WARN line with count. Individual paths at DEBUG.
13. These are actionable — no retry. User must fix permissions.

**Phase 5: Recheck (watch mode)**

14. Each sync pass, engine calls `recheckLocalPermissions(ctx)`:
    a. For each directory-level `local_permission_denied` in sync_failures:
    b. `os.Open(dir)` — if now readable → delete the sync_failures entry, clear the scope block.
    c. Tracker releases all held actions under that directory.
15. If the directory becomes readable between sync passes (user ran `chmod`), the next pass auto-clears.

**Phase 6: User Experience**

16. `onedrive issues`:
    ```
    local_permission_denied (1 directory, ~100,000 files affected):
      /data/protected/ — directory not readable.
      ACTION: Fix permissions with chmod/chown.
    ```
17. Or for individual files:
    ```
    local_permission_denied (100,000 files):
      secret.key — file not readable. Fix permissions with chmod/chown.
      credentials.json — file not readable. Fix permissions with chmod/chown.
      ... and 99,998 more (use --verbose to list all)
    ```

**Scope:** Directory-level (if directory unreadable) or file-level (if individual files). No retry. Recheck on each sync pass for directory-level issues. Workers available for all other actions.

### Journey 5 (TO-BE): File Too Large — 100k files

Identical flow to Journey 1 (Invalid Filename). These are observation-time filters in the scanner — the retry architecture is not involved.

1. Scanner checks file size during walk. Files exceeding 250 GB: `SkippedItem{Reason: "file_too_large", Detail: "312 GB exceeds 250 GB limit"}`.
2. Engine records as actionable failures. One WARN line with count.
3. No actions planned. No worker time. No scope escalation.
4. Auto-clear when user excludes via `skip_files` config or deletes the file.
5. `onedrive issues` groups under `file_too_large` with action text: "Exclude via skip_files in config."

### Journey 6 (TO-BE): Path Too Long — 100k files

Same flow as Journeys 1 and 5.

1. Scanner checks path length. Paths exceeding 400 characters: `SkippedItem{Reason: "path_too_long", Detail: "423 chars exceeds 400-char limit"}`.
2. Engine records as actionable failures. One WARN line with count.
3. No actions planned. No worker time. No scope escalation.
4. Auto-clear when user shortens directory structure.
5. `onedrive issues` groups under `path_too_long` with action text: "Shorten the directory structure or file name."

### Journey 7 (TO-BE): OneDrive Outage (5xx) — 100k files

This is the primary retry + scope scenario. The system must detect an outage, stop wasting resources, and recover efficiently.

**Phase 1: First Workers Hit 5xx**

1. Tracker dispatches actions 1-8 to workers.
2. Worker 1 calls graph API (`SyncTransport`, 0 retries) → HTTP 502 Bad Gateway.
3. Graph client returns `GraphError{StatusCode: 502}`. No transport retry.
4. Worker 1 sends `WorkerResult{HTTPStatus: 502, Success: false}` to engine. Worker 1 immediately pulls next action.

**Phase 2: Transient Retry via Tracker**

5. Engine `classifyResult()` → 502 → `resultTransient`.
6. Engine completes action 1 in tracker, records failure in sync_failures with `next_retry_at = now + 1s`, kicks reconciler.
7. Engine `updateScope()`: adds to sliding window. 1 of 5 needed for service-scope.
8. Workers 2-5 also fail with 5xx. Engine completes each, records each in sync_failures.
9. After result 5: 5 consecutive 5xx from different files within 30 seconds.

**Phase 3: Service-Scope Block**

10. Engine sets `ScopeBlock{Key: "service", IssueType: "service_outage", NextTrialAt: now+60s}`.
11. Tracker moves ALL actions (all accounts, all action types) to held queue.
12. Workers 6-8 were in-flight (race, bounded by 8). They complete, results route through standard path, get held.
13. All workers idle. Total wasted requests: 5 (scope detection) + up to 8 (in-flight race) = 13. Not 2,000,000.

**Phase 4: Trial Actions**

14. Actions 1-5 (from step 6) are recorded in sync_failures with `next_retry_at` timestamps. When the reconciler re-injects them, they flow through buffer → planner → tracker. If the scope block is still active, the tracker holds them in the held queue until a trial succeeds.
15. 60 seconds pass. Trial timer fires.
16. Tracker releases ONE action as trial. Worker executes → 502 (outage continues).
17. Engine: trial failed, same scope error. `TrialInterval` doubles to 120s. Re-hold action.
18. 120 seconds pass. Trial → 502. Interval doubles to 240s. Capped at 600s (10 min).
19. Outage persists for 30 minutes. ~8 trial requests total.

**Phase 5: Recovery**

20. Microsoft recovers. Next trial action → 200 success.
21. Engine clears service-scope block. Trial committed (real work done).
22. Tracker releases all held actions, 8 at a time.
23. Workers process the full backlog. Each action gets a fresh single-attempt dispatch via graph client.
24. If some actions fail (service flapping), scope re-blocks with the same mechanism.

**Phase 6: HTTP 400 "ObjectHandle is Invalid" (special case)**

25. During multi-hour outages, Microsoft sometimes returns HTTP 400 with `innerError.code == "invalidRequest"` and message containing "ObjectHandle is Invalid."
26. Engine: parse error body. If message matches known outage signature → classify as `resultTransient` (not `resultSkip` as it would be for a generic 400).
27. Same scope detection applies. 5 consecutive matching 400s → service-scope block.

**Phase 7: 503 with Retry-After (special case)**

28. If 503 includes `Retry-After: 120`, engine treats this as a server signal (like 429).
29. Immediate service-scope block with `NextTrialAt = now + 120s`.
30. No pattern detection needed — server explicitly said "wait 120 seconds."

**Phase 8: User Experience**

31. `onedrive sync`:
    ```
    WARN Microsoft Graph API unavailable (HTTP 502). Sync paused. (100,000 actions pending)
    INFO Trying again in 60s...
    INFO Trial failed (502). Next trial in 120s...
    INFO Microsoft Graph API recovered. Resuming sync.
    ```
32. `onedrive issues` during outage:
    ```
    service_outage (100,000 actions pending):
      Microsoft Graph API appears unavailable (HTTP 502).
      No action needed — will auto-resolve when service recovers.
      Next trial: in 2m.
    ```

**Scope:** Service-level (all drives including shortcuts, all action types). Workers idle. Microsoft Graph API is shared infrastructure — a 5xx outage affects requests to any drive (own or shared). Shortcut observations (which query the sharer's drive via the same Graph API endpoint) are also blocked. Trial actions use real sync operations targeting any drive. Recovery is immediate when trial succeeds. Total wasted requests during 30-minute outage: ~20 (vs 2M currently).

### Journey 8 (TO-BE): Case Collision — 100k pairs

Observation-time detection, similar to Journey 1 but with per-directory analysis.

**Phase 1: Detection**

1. During scanner walk (or upload validation), engine builds a case-insensitive name map per directory.
2. When two paths in the same directory differ only in case (e.g., `Report.txt` and `report.txt`):
   a. Both flagged as `SkippedItem{Reason: "case_collision", Detail: "conflicts with 'report.txt'"}`.
   b. Neither enters `ScanResult.Events`. Neither is synced.

**Phase 2: Recording**

3. Engine records both paths as actionable failures:
   ```sql
   INSERT INTO sync_failures (path, issue_type, category, error_message)
   VALUES ('dir/Report.txt', 'case_collision', 'actionable', "conflicts with 'report.txt'")
   ```
4. Aggregated logging: 1 WARN line with count.

**Phase 3: No Silent Data Loss**

5. Neither file is uploaded. This prevents the scenario where one silently overwrites the other on OneDrive (which is case-insensitive).
6. No retry. User action required.

**Phase 4: Resolution**

7. User renames `Report.txt` to `report-final.txt`.
8. Next scan: `report-final.txt` and `report.txt` no longer collide.
9. `ClearResolvedActionableFailures` deletes both collision entries.
10. Both files become normal `ChangeEvent`s → planned → uploaded.
11. No manual `issues clear` needed.

**Phase 5: User Experience**

12. `onedrive issues`:
    ```
    case_collision (100,000 pairs):
      dir/Report.txt ↔ dir/report.txt — differ only in case. OneDrive is case-insensitive.
      dir/Data.csv ↔ dir/data.csv — differ only in case.
      ... and 99,998 more pairs (use --verbose to list all)
      ACTION: Rename one file in each pair to avoid the collision.
    ```

**Scope:** File-scoped (per colliding pair). No impact on other workers, drives, or accounts. No scope escalation.

---

## Part 11: Shared Drive Awareness — Full Requirements

A single engine handles the user's primary drive AND shortcuts to shared folders. Each shortcut targets a different drive (the sharer's) with independent quota, independent permissions, and independent content — but shares the same OAuth token and Graph API endpoint. This creates a multi-scope environment within one engine that every failure handling component must respect.

This part consolidates all shared-drive-aware requirements identified during the redesign analysis. Requirements are grouped by concern; they appear in the Part 10 inventory tables under their R-number.

### 11.1: Scope Classification & Routing

Every failure must be routed to the correct scope. The target drive identity is the key input.

**R-2.10.16:** Every `WorkerResult` shall carry the target drive identity (`TargetDriveID`, `ShortcutKey`) so the engine can route failures to the correct scope. Actions targeting the user's own drive carry an empty `ShortcutKey`; shortcut actions carry `remoteDrive:remoteItem`.

**R-2.10.17:** The engine's `updateScope()` shall use target drive context to determine the scope key for 507 and 403 errors. 429 and 5xx shall always route to account-level or service-level scope keys regardless of target drive.

**R-2.10.18:** The engine shall maintain independent sliding windows per scope key for pattern-based scope detection. Three consecutive 507s on own-drive uploads shall not count toward a shortcut's scope window, and vice versa.

### 11.2: Quota Scoping (507)

The user's quota and the sharer's quota are independent. A full user drive does not mean a shared folder is full, and vice versa.

**R-2.10.19:** HTTP 507 on an own-drive action shall set scope key `quota:own`, blocking uploads to all own-drive paths. Shortcut uploads, downloads, deletes, and moves shall continue.

**R-2.10.20:** HTTP 507 on a shortcut action shall set scope key `quota:shortcut:$remoteDrive:$remoteItem`, blocking uploads to that specific shortcut only. Own-drive uploads, other shortcut uploads, and all non-upload actions shall continue.

**R-2.10.21:** Trial actions for a `quota:own` scope block shall select an own-drive upload from the held queue. Trial actions for a `quota:shortcut:*` scope block shall select an upload targeting that specific shortcut. A trial targeting the wrong quota owner proves nothing.

**R-2.10.22:** When multiple shortcuts exist and one hits 507, the `issues` display shall identify the specific shortcut by its local path name (e.g., "Shared folder 'Team Docs'"), not by opaque drive IDs.

### 11.3: Permission Scoping (403)

Each shortcut has independent permissions from an independent owner. A 403 on one shared folder says nothing about another.

**R-2.10.23:** HTTP 403 on a shortcut action shall scope the permission boundary to that shortcut's remote drive. The existing `walkPermissionBoundary` shall use the shortcut's `RemoteDriveID` for the Graph API permission query, not the engine's primary drive.

**R-2.10.24:** A 403 on shortcut A shall not affect shortcut B, even if both are in the same engine. Each shortcut has independent permissions from an independent owner.

**R-2.10.25:** When a shortcut's root itself is read-only (the entire shared folder is read-only), the system shall record the scope block at the shortcut root level and suppress all writes under it, rather than walking up beyond the shortcut boundary.

### 11.4: Rate Limiting (429) — Cross-Drive

429 is the exception: it affects ALL drives equally because the bottleneck is the OAuth token, not a per-drive resource.

**R-2.10.26:** HTTP 429 shall block all action types on all drives within the engine, including all shortcuts. The scope key `throttle:account` applies to every action regardless of target drive, because all API calls share the same OAuth token.

**R-2.10.27:** When a 429 scope block clears (trial success), ALL held actions — own-drive and shortcut alike — shall be released simultaneously. There is no per-drive trial needed for 429 since the bottleneck is the token, not a per-drive resource.

### 11.5: Service Outage (5xx) — Cross-Drive

Like 429, service outages affect shared infrastructure. All drives are equally affected.

**R-2.10.28:** HTTP 5xx scope blocks shall affect all drives including shortcuts. Microsoft Graph API is shared infrastructure; a 5xx on any target drive implies the service is degraded for all.

**R-2.10.29:** The service-scope sliding window shall accept 5xx from any target drive (own or shortcut) as contributing to the pattern. Five consecutive 5xx — even if from different drives — within 30 seconds triggers service-scope block.

### 11.6: Shortcut Observation During Scope Blocks

Shortcut observations make Graph API calls. During scope blocks, these calls may be wasteful or harmful.

**R-2.10.30:** When a `throttle:account` or `service` scope block is active, the engine shall also suppress shortcut observation polling (which makes Graph API calls to `RemoteDriveID`). Observation during a scope block wastes a request and may worsen throttling.

**R-2.10.31:** When a `quota:shortcut:$drive:$item` scope block is active, observation of that specific shortcut shall continue normally (observation is read-only, quota only affects writes). Observation of other shortcuts and the primary drive shall also continue.

### 11.7: User-Facing Display

Different scope owners require different user actions. The display must make this clear. Table 3 (User Communication) in the taxonomy is the canonical per-error-type reference — scope variants are separate rows there.

**R-6.6.11 (quota, own drive):** reason text "Your OneDrive storage is full ({used}/{total})", action text "Free space on OneDrive (delete files, empty recycle bin) or upgrade storage plan."

**R-6.6.11 (quota, shortcut):** reason text "Shared folder '{name}' owner's storage is full", action text "Contact the folder owner to free space or upgrade their storage plan."

**R-6.6.11 (permission, shortcut):** reason text "Shared folder '{name}' is read-only", action text "Request write access from '{owner}', or accept read-only sync for this folder."

**R-2.3.8:** The `issues` command shall sub-group scope-level failures (507, 403) by scope key, showing the drive or shortcut name and scope-owner-specific action text. Different scopes with different owners must not be conflated under a single heading.

**R-2.3.9:** The `issues` command shall display shortcut-scoped failures using the shortcut's local path name (human-readable), not internal drive IDs or scope keys.

**R-2.10.32:** The `status` command (future) shall show per-scope block status: "Uploads paused: your drive (quota full)" and "Uploads paused: 'Team Docs' (owner's quota full)" as separate entries.

### 11.8: Sync Failures Store

The database must capture enough context for per-scope grouping.

**R-2.10.33:** The `sync_failures` table shall store a `scope_key` column for scope-level failures, enabling the `issues` command to group and display by scope without re-deriving scope from path analysis.

**R-2.10.34:** The `scope_key` shall use human-resolvable format in storage: `quota:own`, `quota:shortcut:{localPath}`, `perm:remote:{localPath}`. The `issues` command reads the scope key directly for display grouping.

### 11.9: Data Flow & Pipeline Integrity

Target drive identity must flow through the pipeline without lookup.

**R-6.8.12:** The target drive identity shall flow through the entire action pipeline without lookup: planner sets it on `Action`, tracker preserves it on `TrackedAction`, worker copies it to `WorkerResult`, engine reads it in `classifyResult`/`updateScope`. No component shall query drive identity from the database or API during failure handling.

**R-6.8.13:** The `Action` struct shall expose `TargetsOwnDrive() bool` and `ShortcutKey() string` methods. These are the only drive-identity accessors needed by the tracker (scope matching) and engine (scope routing). Internal representation (drive IDs, remote item IDs) is encapsulated.

### 11.10: Cross-Engine Independence

Engines are independent. No shared scope state.

**R-2.10.35:** Engines shall NOT coordinate scope blocks across engine boundaries. If Engine A's quota is full (507), Engine B discovers independently on its next own-drive upload attempt. The bounded waste (one failed request per engine) is accepted.

**R-2.10.36:** If Engine A hits 429 (account-scoped), Engine B discovers independently on its next API call (same OAuth token). No shared state between engines. Waste: one request per engine.

**R-2.10.37:** Shortcut scope blocks (quota, permissions) are engine-internal. A shortcut in Engine A has no effect on Engine B's shortcuts, even if they point to the same sharer's drive. Each engine maintains its own `ScopeState`.

### 11.11: Edge Cases

Corner cases that arise from the multi-scope environment.

**R-2.10.38:** When a shortcut is removed (unshared or deleted) while a scope block exists for it, the engine shall clear the scope block and discard held actions for that shortcut. Held actions for a removed shortcut are stale and should not be retried.

**R-2.10.39:** When the same sharer's drive is reachable via two different shortcuts in the same engine, 507 on one shortcut shall NOT automatically block the other. Each shortcut has its own scope key (`quota:shortcut:$drive:$itemA` vs `quota:shortcut:$drive:$itemB`). Even though they share a quota owner, the scope detection should discover the second shortcut's block independently — it's the engine's job to detect patterns, not to infer quota relationships.

**R-2.10.40:** When a shortcut targets a folder deep within a shared drive, and a 403 occurs, `walkPermissionBoundary` shall not walk above the shortcut's root item. The shortcut root is the natural boundary — the user has no visibility or control above it.

**R-6.7.21a:** When classifying errors, the engine shall handle the case where `TargetDriveID` is empty (e.g., local-only operations like `os.ErrPermission`) by skipping remote scope routing entirely. Only remote API errors (HTTP status codes) require drive-aware scope routing.

---

## Part 12: Gap Analysis & Resolutions

Cross-reference of the redesign against all sync design docs, actual codebase, and full requirements set. Each gap is assessed, resolved, and either incorporated or dismissed with rationale.

### 12.1: DepTracker Integration Point (Resolved — §7.6 updated)

**Gap.** The redesign treated "Tracker" as a unified scheduler but the actual codebase has `DepTracker` — a dependency graph where `dispatch()` is called when `depsLeft` reaches 0. The pseudocode assumed a top-level dispatch entry point.

**Resolution.** Updated §7.6 to specify that scope and timing gates wrap the existing `dispatch()` call point — AFTER dependency resolution, not as a replacement. The dispatch flow is: dependency satisfied → scope gate → timing gate → ready channel.

**Dependency cascade after scope release:** When a scope-held folder create is released and succeeds, its `Complete()` triggers dependents. These dependents enter `dispatch()` normally and hit the scope/timing gates. If the scope just cleared, they dispatch immediately. If quota re-fills, they fail, scope re-blocks (bounded waste of worker-count requests). This is the ketchup bottle — correct behavior, not a bug.

### 12.2: Ketchup Bottle Effect (Resolved — YAGNI)

**Gap.** When a scope block clears, all held actions become eligible simultaneously. Concern: this could overwhelm the API or re-trigger the scope block.

**Resolution.** Not a problem. R-2.10.11 stays: "release all held actions immediately."

The scope detection mechanism IS the safety valve. If releasing 100k uploads fills the quota:
- Next 3 uploads hit 507 → scope re-blocks in <1 second.
- Waste: 3-8 requests per oscillation (worker count bound).
- User frees more space → cycle repeats. This is the expected workflow.

For 429: single response re-triggers immediately. For 5xx: 5 failures re-detect. Both are fast enough that staggering adds complexity without benefit.

### 12.3: Download/Delete Success Clearing sync_failures (Resolved — new requirement)

**Gap.** Upload success clears sync_failures, but download and delete successes do not auto-clear. Stale failure entries persist after successful retries.

**Resolution.** Added R-2.10.41: success on any action type clears the corresponding sync_failures entry.

### 12.4: Reconciler and Scope Interaction (Resolved — status quo is correct)

**Gap.** Reconciler re-injects items from sync_failures via buffer → planner → tracker. If scope is blocked, items go through the full planner pipeline before being held in the tracker.

**Analysis of three alternatives:**

**Alternative A: Reconciler is scope-unaware (status quo).** Reconciler injects via buffer → planner → tracker. Tracker holds scope-blocked items. HasInFlight() prevents double-injection.
- Pro: Clean separation, no coupling.
- Pro: Planner safety checks (big-delete, conflict detection) run on re-injected items — correct, because files may have changed.
- Pro: sync_failures exponential backoff naturally staggers injection (items don't all fire at once).
- Con: CPU waste from planning items that will be immediately held. Bounded.

**Alternative B: Reconciler checks scope before injecting.** Adds `ScopeChecker` interface.
- Con: Doesn't help the restart case (scope state is empty on restart — reconciler would inject everything anyway).
- Con: Couples reconciler to scope state.

**Alternative C: Reconciler injects directly into tracker (skip planner).**
- Con: Bypasses planner safety checks (big-delete protection, conflict detection). **Unacceptable.** *Eng Philosophy: "Architecture should be extremely robust"* — the planner is the safety gate, skipping it loses guarantees.

**Decision: Alternative A.** The waste is CPU (planning held items), not network (wasted API calls). Planner safety checks must run on re-injected items. sync_failures backoff staggering provides natural rate limiting.

### 12.5: Watch Mode Scope Persistence Across Passes (Resolved — not a gap)

**Gap claimed.** Scope state might not persist across "passes" in watch mode.

**Resolution.** This was a misunderstanding of the architecture. Watch mode (`RunWatch()`, engine.go:817-928) is **fully event-driven** — a `select` loop waiting on debounced batches, reconciler timers, and observer errors. There are no "passes" or "ticks." The Engine struct persists for the entire `RunWatch()` lifecycle. `ScopeState` lives on the Engine and persists naturally. The tracker is persistent (`NewPersistentDepTracker`). No gap exists.

For one-shot mode: the Engine struct persists across multiple `RunOnce()` invocations (e.g., cron). Scope state carries forward.

### 12.6: Scope Detection Window Implementation (Resolved — §7.3.1 added)

**Gap.** "Consecutive" is ambiguous with concurrent workers. The redesign didn't specify how the sliding window handles interleaved success/failure from 8 concurrent workers.

**Resolution.** Added §7.3.1 with implementation detail. Key design: a success from any path in the scope resets the unique-path failure counter. This prevents false scope blocks from interleaved results. 429 and 503-with-Retry-After bypass pattern detection entirely (server signals, immediate scope block).

### 12.7: 401 Token Refresh with SyncTransport (Resolved — §7.6 updated)

**Gap.** With `SyncTransport` (`MaxAttempts: 0`), the first 401 would return as fatal without attempting token refresh. Token refresh is not a transient retry — it's auth lifecycle.

**Resolution.** Updated §7.6 graph client section. `SyncTransport` (0 retries) still performs transparent token refresh on 401 inside `doOnce()`. The `MaxAttempts: 0` policy means "no retries for transient HTTP errors," not "no auth handling." If refresh fails, return `ErrUnauthorized` (fatal).

### 12.8–12.10, 12.12: Dismissed Gaps

| # | Gap | Verdict | Rationale |
|---|-----|---------|-----------|
| 12.8 | 412 backoff too slow (1s vs ms) | Dismissed | 412 is rare. 1s is not a meaningful cost. Premature optimization. |
| 12.9 | Partial delta fetch during scope block | Dismissed | Already handled: delta token not advanced until all pages complete. Partial state harmlessly discarded. |
| 12.10 | Remote observer calls `isValidOneDriveName()` | Dismissed | OneDrive enforces its own naming rules server-side. Applying upload rules to downloads is wrong. **Action:** remove the call from observer_remote.go:405. Path traversal safety is R-6.4.8. |
| 12.12 | Big-delete + scope-held actions | Dismissed | Big-delete fires when it fires. Safety threshold exists for a reason. |

### 12.11: Scope Persistence on Restart (Resolved — don't persist)

Don't persist any trial-only scope blocks. Re-detection cost: 3-5 HTTP requests per scope after restart. Persisting adds schema complexity and risks stale blocks preventing work after conditions change during downtime. Permission-based scopes are already effectively persisted via `sync_failures` entries (`permission_denied`, `local_permission_denied`) — `recheckPermissions()` runs at startup.

| Scope | Persist? | Rationale |
|-------|----------|-----------|
| `throttle:account` (429) | No | Retry-After is 1-120s. Expired by restart time. |
| `service` (5xx) | No | Transient. Persisted block would be stale if service recovered. |
| `quota:*` (507) | No | Re-detection: 3 requests. sync_failures + reconciler rate-limit injection naturally. |
| `perm:*` (403/local) | Already handled | sync_failures entries persist. Engine rechecks at startup. |

### 12.13–12.14: Resolved Elsewhere

**12.13 (disk_full):** Design in Part 13. **12.14 (circuit breaker dead code):** Delete `circuit.go`, `circuit_test.go`, update `doc.go`. Scope-based blocking supersedes circuit breaker.

---

## Part 13: Disk Space Protection

### 13.1: disk_full Detection Design

Implements R-6.2.6 (check available disk space before downloading) and R-6.4.7 (configurable `min_free_space`).

**Detection point.** Pre-check in the executor, before starting the download. The executor knows the file's remote size (from action metadata) and can check local disk space cheaply (`syscall.Statfs` / `unix.Statfs`).

```go
func (ex *Executor) executeDownload(ctx context.Context, action Action) Outcome {
    remoteSize := action.View.Remote.Size
    available := diskFreeSpace(ex.syncRoot)
    minFree := ex.config.MinFreeSpace  // default 1 GB

    if available < uint64(minFree) {
        // Critically low: even small files shouldn't download
        return Outcome{
            Success: false,
            Error:   fmt.Errorf("%w: available %s < min_free_space %s",
                ErrDiskFull, humanize(available), humanize(minFree)),
        }
    }
    if available < uint64(remoteSize) + uint64(minFree) {
        // This specific file doesn't fit, but smaller files might
        return Outcome{
            Success: false,
            Error:   fmt.Errorf("insufficient space for file: need %s, available %s",
                humanize(remoteSize), humanize(available - uint64(minFree))),
        }
    }
    // ... proceed with download
}
```

**Two-level detection:**

1. **Critical: available < min_free_space.** Scope-level signal. Scope key: `disk:local`. Blocks ALL downloads (even small files risk violating the reservation). Detection: immediate from a single `ErrDiskFull` (deterministic, like 429 — not a pattern to infer). Trial timing: 5 minutes, 2× backoff, max 1 hour.

2. **Per-file: available ≥ min_free_space but < file_size + min_free_space.** Per-file failure only. This specific file doesn't fit, but smaller files may. No scope escalation. Issue type: `file_too_large_for_space`. The per-file check runs naturally on each download attempt.

**Scope behavior:**

| Property | Value |
|----------|-------|
| Scope key | `disk:local` |
| Blocks | Downloads only. Uploads and deletes unaffected. |
| Detection | Immediate from single `ErrDiskFull` (available < min_free_space). Deterministic signal. |
| Trial timing | First at 5 min, 2× backoff, max 1 hour. |
| Trial action | Real download. Pre-check runs again. If space now available → succeeds → scope clears. |
| Issue type | `disk_full` |
| Category | `actionable` (user must free space) |
| User display | "Local disk space below minimum ({available} free, {min_free_space} required). Downloads paused." |
| User action | "Free local disk space." |

**User experience:**

```
WARN Local disk critically low (500 MB free, min_free_space=1 GB). Downloads paused.
```

`onedrive issues`:
```
disk_full (50,000 downloads pending):
  Local disk below minimum (500 MB free, 1 GB required).
  ACTION: Free local disk space.
  Next check: in 5m.
```

**Why pre-check, not post-failure.** A failed write (disk full mid-download) leaves a `.partial` file and requires cleanup. Pre-checking before the download avoids partial files, avoids wasted bandwidth, and provides a clean error. *Eng Philosophy: "Architecture should be extremely robust and full of defensive coding practices."*

**Configuration.** `min_free_space` defaults to 1 GB. Set to 0 to disable (downloads proceed until disk is truly full). The default is conservative — 1 GB prevents the sync from filling the disk and breaking other applications.

---

## Part 14: Removal Inventory

Everything that gets deleted during this redesign. *Eng Philosophy: "Ensure after refactoring the code doesn't show any signs of the old architecture."*

### Code Deletions

| File | What to Delete | Why |
|------|---------------|-----|
| `internal/retry/circuit.go` | **Entire file** | Dead code. Never imported in production. Scope-based blocking supersedes circuit breaker. |
| `internal/retry/circuit_test.go` | **Entire file** | Tests for dead code. |
| `internal/retry/doc.go` | Circuit breaker documentation section | References deleted code. |
| `internal/retry/named.go` | `Action` policy definition | Executor retry loop removed — no consumer. |
| `internal/sync/executor.go` | `withRetry()` function | Retry loop replaced by engine classification + sync_failures + reconciler. |
| `internal/sync/executor.go` | `classifyError()` function | Classification moved to engine `classifyResult()`. |
| `internal/sync/executor.go` | `classifyStatusCode()` function | Classification moved to engine `classifyResult()`. |
| `internal/sync/executor.go` | `sleepFunc` field and injection | No more blocking sleep in executor. |
| `internal/sync/executor.go` | `errClassRetryable`, `errClassFatal`, `errClassSkip` constants | Engine uses `resultClass` enum instead. |
| `internal/sync/executor.go` | `executorMaxRetries` constant | No executor retry budget — reconciler handles retry via sync_failures. |
| `internal/sync/executor.go` | 423 `errClassSkip` handling | Reclassified: 423 is transient in engine `classifyResult()`. Non-blocking retry handles multi-hour SharePoint locks. |
| `internal/sync/observer_remote.go` | `isValidOneDriveName()` call (line ~405) | Wrong: applies upload naming rules to downloads. OneDrive enforces its own rules server-side. |
| `internal/sync/upload_validation.go` | **Entire file** | Upload validator deleted. All validation moved to observation layer (`shouldObserve()` + Stage 2 size check). |
| `internal/sync/upload_validation.go` | `filterInvalidUploads()` function | Replaced by `shouldObserve()` in scanner/watch handlers (Stage 1) and size check in `processEntry`/watch handlers (Stage 2). |
| `internal/sync/upload_validation.go` | `validateUploadActions()` function | No longer needed — invalid files never reach the planner. |
| `internal/sync/upload_validation.go` | `validateSingleUpload()` function | No longer needed — validation happens at observation time. |
| `internal/sync/upload_validation.go` | `ValidationFailure` type | Replaced by `SkippedItem` type in `types.go`. |
| `internal/sync/upload_validation.go` | `removeActionsByIndex()` helper | No longer needed — no post-planning action filtering. |
| `internal/sync/store_interfaces.go` | `FailureRecorder` interface | Consolidated into `SyncFailureRecorder`. See Interface Consolidation below. |

### Interface Consolidation

The store has two overlapping failure recording interfaces: `FailureRecorder` (old: `RecordFailure(path, driveID, direction, errMsg, httpStatus)`) and `SyncFailureRecorder` (new: `RecordSyncFailure(path, driveID, direction, issueType, errMsg, httpStatus, fileSize, localHash, itemID)`). The engine's hot path (`processWorkerResult`) calls `RecordFailure`, which always writes `category='transient'` and cannot set `issueType`. The redesigned `classifyResult()` needs a single recording path with `issueType` and `category` for all failure types. Consolidate: all callers use `RecordSyncFailure`. Move the `remote_state` status transition (currently inside `RecordFailure`) into `CommitOutcome` for symmetry — it already handles success transitions; failure transitions belong there too. Delete `FailureRecorder` interface and `RecordFailure` method.

### Explicitly Unchanged

| Component | Why Unchanged |
|-----------|---------------|
| Hash mismatch retry (`driveops/transfer_manager.go downloadWithHashRetry`, max 2 retries) | Data integrity mechanism, not transient error retry. Operates on successful HTTP responses where content hash doesn't match remote. Orthogonal to the failure redesign. The tracker does not see hash retry iterations. The sync_failures + reconciler retry (§7.5) is for transport/API errors only; hash mismatches are a separate concern with their own bounded retry (3 attempts, no backoff). |
| `waitForThrottle()` in graph client | **Demoted from primary gate to defense-in-depth safety net.** The primary 429 gate is the tracker's scope block. `waitForThrottle` catches the race window (up to `transfer_workers` in-flight requests). NOT deleted — retained as a second layer. See §7.6. |

### Policy Deletions

| Policy | Current Use | Replacement |
|--------|------------|-------------|
| `retry.Action` | Executor `withRetry` (3 attempts, 1s-unbounded) | sync_failures + reconciler (single curve: 1s→1h) |
| `retry.CircuitBreaker` | Unused (dead code) | Scope-based blocking with trial actions |

### Behavioral Removals

| Behavior | Current | After Redesign |
|----------|---------|----------------|
| Executor retry loop | 3 attempts with blocking sleep per action | **Removed.** Executor dispatches once, returns result. |
| Transport retry in sync path | 5 HTTP attempts with 1-60s blocking sleep | **Removed for sync.** `SyncTransport` (MaxAttempts: 0) = single HTTP attempt. CLI retains 5-attempt `Transport`. |
| Nested retry multiplication | 5 transport × 4 executor = 20 attempts per action | **Eliminated.** One attempt per dispatch. Failures go to sync_failures immediately for reconciler retry. |
| Three-way error classification | `graph/errors.go isRetryable()` + `executor.go classifyError()` + `engine.go processWorkerResult()` | **Collapsed to one.** Engine `classifyResult()` is the single classification point. |
| Per-action blocking backoff | Worker sleeps 1-60s between retries | **Eliminated.** Workers never sleep. Backoff is in sync_failures `next_retry_at` (non-blocking, reconciler-driven). |
| 423 skip (no retry) | `errClassSkip` — SharePoint locks silently dropped | **Reclassified as transient.** Non-blocking retry via sync_failures + reconciler handles multi-hour locks naturally. Backoff (1s→1h) matches lock durations. |

### Spec/Design Removals

| Document | What Changes |
|----------|-------------|
| `spec/design/retry.md` | Remove `Action` policy. Remove `CircuitBreaker` section. |
| `spec/design/sync-execution.md` | Remove executor retry loop documentation. Remove `classifyError`/`classifyStatusCode` documentation. |

---

## Part 15: Implementation Plan

### Approach

*Eng Philosophy: "Prefer large, long-term solutions over quick fixes. Do big re-architectures early, not late."*

The redesign touches 7 design docs, 5 requirement files, and ~15 production code files. It cannot be done as a single PR without losing reviewability. But it also cannot be split into tiny increments that leave the architecture half-old, half-new — that violates "ensure after refactoring the code doesn't show any signs of the old architecture."

**Strategy: requirements first, then design docs, then code in focused increments.** Each code increment leaves the codebase green (builds, tests pass, lint clean). The core refactor (tracker + engine + executor) is one increment — it's large but it's one coherent architectural change that can't be meaningfully split.

### Phase 0: Requirements (1 PR)

**PR: `docs/failure-handling-requirements`**

Update `spec/requirements/` files with all new and revised requirements from this redesign:
- `spec/requirements/sync.md`: R-2.10.1-44 (failure management), R-2.3.7-9 (issues display), R-2.11.5-6 (filename validation), R-2.12.1-2 (case collision)
- `spec/requirements/non-functional.md`: R-6.6.7-11 (observability), R-6.8.4-14 (network resilience), R-6.2.6/R-6.4.7 (disk space), R-6.7.14/21a (technical)

Status for all: `[planned]`. This PR establishes the requirements baseline. No code changes.

Post-taxonomy additions (added after initial Phase 0 scope):
- R-6.8.15: Transient error classification and issue_type assignment (5xx, 408, 412, 404, 423)
- R-6.6.12: Aggregated transient failure logging (same pattern as R-6.6.7 for execution-time failures)
- R-2.10.34: Dual scope key format (internal IDs in ScopeState, local paths in sync_failures)
- context.Canceled/DeadlineExceeded: Graceful shutdown handling in classifyResult
- R-6.8.5: RateLimit-Remaining component assignment (deferred to post-launch)

### Phase 1: Design Docs (1 PR)

**PR: `docs/failure-handling-design`**

Update `spec/design/` files per the Design Document Changes table in Part 10. Each section that describes to-be behavior is marked with `Implements: R-X.Y.Z [planned]` — status stays `[planned]` until code is written and tested.

### Phase 2: Foundation (1 PR)

**PR: `chore/failure-handling-foundation`**

Non-breaking preparatory changes. All existing tests continue to pass unchanged.

1. **Delete dead code:** `internal/retry/circuit.go`, `circuit_test.go`. Update `doc.go`.
2. **Add `SyncTransport` policy:** `internal/retry/named.go`. New policy, no existing code uses it yet.
3. **Parameterize graph client retry policy:** Add `retryPolicy retry.Policy` field to `Client` struct. Accept as constructor parameter (default: `retry.Transport`). Replace hardcoded `retry.Transport.MaxAttempts` in `doRetry` (lines 124, 166) and `doPreAuthRetry` (lines 318, 352) with `c.retryPolicy.MaxAttempts`. All existing callers unchanged (pass default or nothing).
4. **Add `RetryAfter` to `GraphError`:** `internal/graph/errors.go`. Parse `Retry-After` header for 429 and 503 into the field.
5. **Add 401 token refresh in `doOnce()`:** `internal/graph/client.go`. On 401, refresh token, retry once. If still 401, return `ErrUnauthorized`. This is a bugfix — currently 401 is terminal with no refresh attempt.
6. **Store extensions — scope_key:** Add `scope_key TEXT NOT NULL DEFAULT ''` column to `sync_failures` (migration). Add `ScopeKey string` field to `SyncFailureRow`. Add `scopeKey` parameter to `RecordSyncFailure`. Update all INSERT/UPSERT queries to include `scope_key`. Update all SELECT queries to read `scope_key`. Add `UpsertActionableFailures` batch method. Add `ClearResolvedActionableFailures`.
7. **Store extensions — success cleanup + interface consolidation:** Extend `CommitOutcome` to clear sync_failures for download/delete/move successes (currently upload-only). Move `remote_state` failure status transitions from `RecordFailure` into a new method callable from both success and failure paths. Delete `FailureRecorder` interface and `RecordFailure` method — all callers use `RecordSyncFailure`.
8. **New issue types:** `quota_exceeded`, `local_permission_denied`, `case_collision`, `disk_full`, `service_outage`, `file_too_large_for_space`.
9. **Remove `isValidOneDriveName()` from remote observer:** observer_remote.go line ~405. Simple deletion. [implemented]
10. **Fix isActionableIssue():** baseline.go `isActionableIssue()` (lines 1511-1519) does not include `IssuePermissionDenied`. Currently `permission_denied` is defined as a constant but not checked in `isActionableIssue()` — permission failures are misclassified as transient and scheduled for retry instead of being treated as actionable.

TDD: Write tests for new store methods, 401 refresh, RetryAfter parsing, SyncTransport policy, graph client parameterization first.

### Phase 3: Scanner ScanResult (1 PR)

**PR: `feat/scanner-scan-result`**

Scanner returns `ScanResult{Events, Skipped}` instead of `[]ChangeEvent`. Engine processes skipped items. Unified observation filter `shouldObserve()` replaces scattered filter calls. Upload validator deleted entirely.

1. **`ScanResult` and `SkippedItem` types:** `ScanResult{Events []ChangeEvent, Skipped []SkippedItem}` in `types.go`. `SkippedItem{Path, Reason, Detail string}`.
2. **`shouldObserve()` unified filter:** Single entry point in `scanner.go` replacing scattered `isAlwaysExcluded()` + `isValidOneDriveName()` calls across scanner, watch handlers, and watch setup. Stage 1 (name + path length, before stat). Stage 2 (file size > 250 GB, after stat) in `processEntry`/`handleCreate`/`hashAndEmit`/`scanNewDirectory`.
3. **`validateOneDriveName()`:** Returns `(reason, detail)` strings for invalid names. `isValidOneDriveName()` delegates to it.
4. **Scanner changes:** Invalid files → `SkippedItem` instead of silent DEBUG log.
5. **Upload validator deletion:** `upload_validation.go` deleted entirely — `filterInvalidUploads`, `validateUploadActions`, `validateSingleUpload`, `ValidationFailure`, `removeActionsByIndex` all removed. Issue type constants moved to `issue_types.go`.
6. **Watch handler updates:** Watch handlers and watch setup use `shouldObserve()` instead of direct `isAlwaysExcluded()` + `isValidOneDriveName()` calls.
7. **Engine `recordSkippedItems()`:** Groups by reason, batch-upserts to sync_failures as actionable.
8. **Engine `clearResolvedSkippedItems()`:** Deletes sync_failures entries for files no longer skipped.
9. **Aggregated logging:** >10 same-type skips → 1 WARN summary + individual DEBUG. <=10 → per-file WARN.
10. **Directory buffering:** Buffer entries per directory during `filepath.WalkDir` before emitting ChangeEvents. This is a prerequisite for Phase 7 (case collision detection), which requires a per-directory case-insensitive name map. Without buffering, the scanner emits events one-at-a-time during the walk and cannot compare sibling names.

TDD: Write tests for ScanResult contract, engine recording, auto-clear, aggregated logging first.

### Phase 4: Core Refactor (1 PR)

**PR: `refactor/unified-retry-and-scope`**

The big one. This is the architectural heart of the redesign — tracker extensions, engine classification, executor simplification, scope detection. These are deeply coupled and must change together.

**Removals:**
- Delete `withRetry`, `classifyError`, `classifyStatusCode`, `sleepFunc`, `errClass*` from executor
- Delete `retry.Action` policy

**Additions/Changes:**
1. **TrackedAction extensions:** `IsTrial`, `TrialScopeKey`.
2. **Tracker held queue:** Per-scope-key map. Scope gating in `dispatch()` (after dependency resolution).
3. **Tracker `releaseScope()`:** Release all held actions for a scope key.
4. **Tracker `dispatchTrial()`:** Release one action marked IsTrial from held queue.
7. **WorkerResult extensions:** `TargetDriveID`, `ShortcutKey`, `RetryAfter`. Worker populates from action.
8. **Engine `classifyResult()`:** Single classification point. Maps HTTP codes + error types → result class.
9. **Engine `updateScope()`:** Target-drive-aware routing. Sliding window detection. ScopeBlock management.
10. **`ScopeState`:** Blocks map, windows, trial timers.
11. **Executor simplification:** Action methods call graph client directly, return Outcome. No retry loop. Hash mismatch retry (`downloadWithHashRetry`) is unchanged — it's a data integrity mechanism inside the transfer manager, orthogonal to this redesign.
12. **Graph client sync callers:** Use `SyncTransport` (0 retries) instead of `Transport`. This applies to both `doRetry` and `doPreAuthRetry` (upload chunks, download streams) via the parameterized policy from Phase 2.
13. **Engine result recording:** `processWorkerResult` calls `RecordSyncFailure` (from Phase 2 interface consolidation) instead of the deleted `RecordFailure`.

TDD: Extensive test suite for scope gating, held queues, trial dispatch, engine classification, scope detection windows, WorkerResult routing, sync_failures recording and reconciler re-injection.

### Phase 5: Specific Scopes (1 PR)

**PR: `feat/disk-full-and-local-permissions`**

Add the two remaining scope types that aren't covered by the core refactor.

1. **disk_full:** Pre-check in executor before download. Two-level: critical (< min_free_space → `disk:local` scope) vs per-file. `min_free_space` config option (default 1 GB).
2. **Local permission hierarchical check:** `os.ErrPermission` → check parent directory → directory-level scope block or file-level failure. Recheck on each sync pass.

TDD: Tests for disk space pre-check, scope block, trial download, min_free_space config, directory permission detection, auto-clear on recheck.

### Phase 6: User-Facing Display (1 PR)

**PR: `feat/issues-display-and-logging`**

User-facing improvements to issues command and logging.

1. **Issues grouped display:** >10 items of same type → grouped heading with count, first 5 paths, `--verbose` for all.
2. **Per-error-type reason/action text:** Plain-language reason + concrete user action for each issue type.
3. **Per-scope sub-grouping:** 507/403 grouped by scope (own drive vs each shortcut).
4. **Transient retry log levels:** Individual retries at DEBUG, exhausted at WARN, resolved at INFO.

### Phase 7: Case Collision (1 PR)

**PR: `feat/case-collision-detection`**

Case-insensitive collision detection during scanner walk.

Depends on Phase 3's directory buffering infrastructure. The scanner must buffer entries within each directory before emitting events so that case-insensitive name comparison can happen across siblings.

1. **Per-directory case-insensitive name map** in scanner.
2. **Both colliding files** flagged as `SkippedItem{Reason: "case_collision"}`.
3. **Neither uploaded** until collision resolved.
4. **Auto-clear** when user renames one file.

### Phase Summary

| Phase | PR | Type | Size | Dependencies |
|-------|-----|------|------|--------------|
| 0 | Requirements | Docs only | Small | None |
| 1 | Design docs | Docs only | Medium | Phase 0 |
| 2 | Foundation | Code | Medium | Phase 1 |
| 3 | Scanner ScanResult | Code | Small | Phase 2 |
| 4 | Core refactor | Code | **Large** | Phase 2 |
| 5 | Disk full + local perms | Code | Medium | Phase 4 |
| 6 | Issues display + logging | Code | Medium | Phase 4 |
| 7 | Case collision | Code | Small | Phase 3 |

Phases 3 and 4 can run in parallel (3 depends on 2, 4 depends on 2, but not on each other). Phases 5, 6, 7 can all run in parallel after phase 4.

After each phase: update requirement statuses from `[planned]` → `[implemented]` → `[verified]` as tests pass. Update design doc `Implements:` lines.

---

## Part 10: Complete Requirements Inventory

All proposed new/changed requirements, organized by area. Failure handling principles from the analysis are encoded directly as testable requirements below — not as CLAUDE.md philosophy additions. The Failure Taxonomy tables (above) and this inventory are kept in sync — any change to one must be reflected in the other.

### R-2 Sync — Failure Management (R-2.10)

| ID | Requirement | Status | Part |
|----|------------|--------|------|
| R-2.10.1 | When a transfer fails with HTTP 507, the system shall classify it as an actionable failure with issue type `quota_exceeded`, scoped to the quota owner. For own-drive actions: scope key `quota:own` (user's quota shared across own drives). For shortcut actions: scope key `quota:shortcut:{localPath}` (sharer's independent quota; see R-2.10.34 for dual-format convention). The system shall NOT retry 507 at the transport level. Uploads to the affected scope shall be suppressed; downloads and uploads to other scopes shall continue. | planned (revise) | 7.2, 7.4.1 |
| R-2.10.2 | When a user resolves a file-scoped actionable failure (by renaming, moving, or deleting the file), the system shall automatically detect the resolution on the next scanner pass and remove the stale failure entry. Implementation: engine calls `ClearResolvedActionableFailures` after observation, comparing current skipped paths against recorded failures. | planned (revise) | 4 |
| R-2.10.3 | The system shall detect scope-level failure patterns: 429 immediate account-scope from single response; 507 account-scope after 3 consecutive from different files in 10s; 5xx service-scope after 5 consecutive from different files in 30s; 503 with Retry-After immediate service-scope. Scope blocks prevent the tracker from dispatching actions in the affected scope. | planned (revise) | 7.3 |
| R-2.10.4 | When displaying sync status, the system shall show failure scope context (file, directory, drive/shortcut, account, service) alongside retry information. For per-drive scopes (507, 403), the display shall identify which drive or shortcut is affected and show the appropriate user action for that scope owner. | planned | 7.3, 7.4.1 |
| R-2.10.5 | When a scope block is active, the system shall test for recovery by periodically releasing one real action from the held queue as a trial. On trial success: clear the scope block and release all held actions. On trial failure: re-hold and extend trial interval. No synthetic probe requests. | **new** | 7.3 |
| R-2.10.6 | For `quota_exceeded`: trial timing starts at 5 minutes, doubles on each failed trial, max 1 hour. A successful trial upload proves quota is available and clears the scope. | **new** | 7.3 |
| R-2.10.7 | For `rate_limited`: trial timing starts at `Retry-After` duration from server response. Scope block affects all action types for the account (429 throttles all API calls). | implemented (revise: use trial, not `waitForThrottle` sleep) | 7.3 |
| R-2.10.8 | For `service_outage`: trial timing starts at 60 seconds (or `Retry-After` if present), doubles on failure, max 10 minutes. A successful trial action proves the service is available and clears the scope. | **new** | 7.3 |
| R-2.10.9 | When `recheckPermissions()` discovers a previously-denied folder is now writable, the system shall clear the scope block and release all held actions under that folder immediately. | **new** (enhance existing) | 8 |
| R-2.10.10 | When the scanner observes a previously-blocked file or directory is now accessible, the system shall clear the failure and release held actions. | **new** | 8 |
| R-2.10.11 | When a scope block clears (via trial success or recheck), the system shall release all held actions immediately for dispatch. | **new** | 7.3 |
| R-2.10.12 | When a local file operation fails with `os.ErrPermission`, the system shall check parent directory accessibility. If the directory is inaccessible: record one `local_permission_denied` at directory level and suppress all operations under it. If the directory is accessible: record at file level. | **new** | 8 |
| R-2.10.13 | The system shall recheck `local_permission_denied` directory-level issues at the start of each sync pass, and auto-clear when accessible. | **new** | 8 |
| R-2.10.14 | Trial timing per scope type: `rate_limited` starts at Retry-After (max 10 min); `quota_exceeded` starts at 5 min (2× backoff, max 1 hour); `service_outage` starts at 60s or Retry-After (2× backoff, max 10 min). | **new** | 7.3 |
| R-2.10.15 | When a scope block is set, at most `transfer_workers` actions may be in-flight. These complete normally and route through standard retry. This bounded waste (worker count) is accepted; no locking between result processing and dispatch. | **new** | 7.3 |
| R-2.10.16 | Every `WorkerResult` shall carry target drive identity (`TargetDriveID`, `ShortcutKey`). Own-drive actions: empty `ShortcutKey`. Shortcut actions: `remoteDrive:remoteItem`. | **new** | 11.1 |
| R-2.10.17 | `updateScope()` shall use target drive context for 507/403 scope keys. 429/5xx always route to account/service scope regardless of target drive. | **new** | 11.1 |
| R-2.10.18 | Independent sliding windows per scope key. Own-drive 507s shall not count toward shortcut scope windows, and vice versa. | **new** | 11.1 |
| R-2.10.19 | 507 on own-drive → scope key `quota:own`, blocks own-drive uploads only. Shortcut uploads, downloads, deletes, moves continue. | **new** | 11.2 |
| R-2.10.20 | 507 on shortcut → scope key `quota:shortcut:{localPath}`, blocks that shortcut's uploads only. Own-drive and other shortcuts continue. See R-2.10.34 for dual-format convention. | **new** | 11.2 |
| R-2.10.21 | Trial actions for `quota:own` shall select own-drive uploads. Trial actions for `quota:shortcut:*` shall select uploads targeting that shortcut. A trial targeting the wrong quota owner proves nothing. | **new** | 11.2 |
| R-2.10.22 | `issues` display shall identify shortcut-scoped 507 by local path name (e.g., "Shared folder 'Team Docs'"), not opaque drive IDs. | **new** | 11.2 |
| R-2.10.23 | 403 on shortcut shall scope boundary to that shortcut's `RemoteDriveID`. `walkPermissionBoundary` uses shortcut's drive, not primary. | **new** | 11.3 |
| R-2.10.24 | 403 on shortcut A shall not affect shortcut B. Each shortcut has independent permissions from an independent owner. | **new** | 11.3 |
| R-2.10.25 | When a shortcut root itself is read-only, record scope block at shortcut root level. Do not walk above shortcut boundary. | **new** | 11.3 |
| R-2.10.26 | 429 shall block all action types on all drives including shortcuts (`throttle:account`). Same OAuth token = same rate limit. | **new** | 11.4 |
| R-2.10.27 | When 429 scope clears, ALL held actions (own-drive + shortcuts) released simultaneously. No per-drive trial needed. | **new** | 11.4 |
| R-2.10.28 | 5xx scope blocks affect all drives including shortcuts. Graph API is shared infrastructure. | **new** | 11.5 |
| R-2.10.29 | Service-scope sliding window accepts 5xx from any target drive. Five consecutive 5xx from different drives within 30s triggers block. | **new** | 11.5 |
| R-2.10.30 | During `throttle:account` or `service` scope block, suppress shortcut observation polling (wastes API calls, may worsen throttling). | **new** | 11.6 |
| R-2.10.31 | During `quota:shortcut:*` scope block, observation of that shortcut continues (read-only). Other observations unaffected. | **new** | 11.6 |
| R-2.10.32 | `status` command (future) shall show per-scope block status as separate entries per drive/shortcut. | **new** | 11.7 |
| R-2.10.33 | `sync_failures` table shall store `scope_key` column for scope-level failures. Enables `issues` display grouping without re-deriving scope. | **new** | 11.8 |
| R-2.10.34 | `scope_key` format: `quota:own`, `quota:shortcut:{localPath}`, `perm:remote:{localPath}`. The sync_failures table (persistent, used for issues display) stores human-readable local paths (e.g., `quota:shortcut:Team Docs`). ScopeState (in-memory, used for scope matching in the tracker) uses stable internal identifiers (e.g., `quota:shortcut:$remoteDrive:$remoteItem`). Translation happens once at recording time — the engine knows both formats. | **new** | 11.8 |
| R-2.10.35 | Engines shall NOT coordinate scope blocks across engine boundaries. Each discovers independently. Bounded waste accepted. | **new** | 11.10 |
| R-2.10.36 | 429 discovered independently per engine (same token). No shared state. Waste: one request per engine. | **new** | 11.10 |
| R-2.10.37 | Shortcut scope blocks are engine-internal. A shortcut in Engine A has no effect on Engine B's shortcuts. | **new** | 11.10 |
| R-2.10.38 | When a shortcut is removed while a scope block exists for it, clear the block and discard held actions. | **new** | 11.11 |
| R-2.10.39 | Two shortcuts to same sharer's drive: 507 on one does NOT auto-block the other. Independent scope keys per shortcut. | **new** | 11.11 |
| R-2.10.40 | `walkPermissionBoundary` on shortcut shall not walk above shortcut root. Shortcut root is the natural boundary. | **new** | 11.11 |
| R-2.10.41 | When a download, delete, or move action succeeds, the system shall clear any corresponding `sync_failures` entry for that path, matching the auto-clear behavior already implemented for uploads. All action types must clear on success. | **new** | 12.3 |
| R-2.10.42 | The scope detection sliding window shall accept results from concurrent workers. A success from any path in the scope resets the unique-path failure counter. This prevents false scope blocks from interleaved results. Pattern-based detection requires N unique-path failures with no intervening success within T seconds. | **new** | 12.6, 7.3.1 |
| R-2.10.43 | `disk_full` scope: when available disk space falls below `min_free_space`, the system shall set a `disk:local` scope block suppressing all downloads. Detection: immediate from single pre-check failure (deterministic signal). Trial: real download at 5 min, 2× backoff, max 1 hour. | **new** | 13.1 |
| R-2.10.44 | When available disk space ≥ `min_free_space` but < file_size + `min_free_space`, the system shall record a per-file failure without scope escalation. Smaller files that fit within available space may still download. | **new** | 13.1 |

### R-2 Sync — Issues Display (R-2.3)

| ID | Requirement | Status | Part |
|----|------------|--------|------|
| R-2.3.7 | When the `issues` command encounters more than 10 failures of the same issue_type, it shall group them under a single heading with count, showing the first 5 paths. `--verbose` shows all. | **new** | 3 |
| R-2.3.8 | For scope-level issues where drives have independent scopes (507 quota, 403 permissions), `issues` shall sub-group by scope (own drive vs each shortcut). Different scopes have different owners and different user actions. | **new** | 3, 11.7 |
| R-2.3.9 | `issues` shall display shortcut-scoped failures using the shortcut's local path name (human-readable), not internal drive IDs or scope keys. | **new** | 11.7 |

### R-2 Sync — Filename Validation (R-2.11)

| ID | Requirement | Status | Part |
|----|------------|--------|------|
| R-2.11.1 | Reject invalid characters before upload. | implemented (update status) | 2 |
| R-2.11.2 | Reject reserved names. | implemented (update status) | 2 |
| R-2.11.3 | Reject `~$` prefix, `_vti_`, `.lock`. | implemented (update status) | 2 |
| R-2.11.4 | Reject trailing dots, leading/trailing whitespace. | implemented (update status) | 2 |
| R-2.11.5 | Scanner-filtered files shall be recorded as actionable issues (via `ScanResult.Skipped` → engine → sync_failures), not silently skipped with only a DEBUG log. | **new** | 2 |
### R-2 Sync — Case Collision (R-2.12)

| ID | Requirement | Status | Part |
|----|------------|--------|------|
| R-2.12.1 | Detect case collisions before upload, flag as actionable `case_collision`. | planned (elevated priority) | 9, J8 |
| R-2.12.2 | Neither colliding file shall be uploaded until the collision is resolved. | **new** | 9, J8 |

### R-6.6 Observability

| ID | Requirement | Status | Part |
|----|------------|--------|------|
| R-6.6.7 | When >10 items share the same warning category in a sync pass, log 1 WARN summary + individual DEBUG. | **new** | 3 |
| R-6.6.8 | Individual retry attempts for transient errors logged at DEBUG, not WARN. Only final outcome logged at WARN or higher. | **new** | 5 |
| R-6.6.9 | Transient errors that resolve within retry budget shall not emit WARN. Log at INFO with attempt count. | **new** | 5 |
| R-6.6.10 | Exhausted retries: single WARN with final error, attempt count, and next retry time. | **new** | 5 |
| R-6.6.11 | Every failure shown to user includes plain-language reason AND concrete user action. Per-error-type reason and action text shall cover all failure categories, with scope-owner-specific variants for shortcut-scoped failures. Table 3 (User Communication) is the canonical per-error-type reference. | **new** | 6, Table 3 |
| R-6.6.12 | When >10 transient failures of the same issue_type exhaust their retry budget in a single sync pass, aggregate into 1 WARN summary with count, individual paths at DEBUG. Extends R-6.6.7 pattern to execution-time transient failures. | **new** | Table 1 |
### R-6.7 Technical Requirements

| ID | Requirement | Status | Part |
|----|------------|--------|------|
| R-6.7.14 | When the Graph API returns HTTP 400 with `innerError.code == "invalidRequest"` and known outage message patterns (e.g., "ObjectHandle is Invalid"), classify as transient (service outage), not generic skip. | planned (revise) | 9, J7 |
| R-6.7.27 | When classifying errors, the engine shall handle empty `TargetDriveID` (local-only operations like `os.ErrPermission`) by skipping remote scope routing. Only remote API errors require drive-aware scope routing. | **new** | 11.11 |
### R-6.2 Data Integrity (Disk Space)

| ID | Requirement | Status | Part |
|----|------------|--------|------|
| R-6.2.6 | Before each download, the system shall verify available disk space ≥ `min_free_space`. If below: set `disk:local` scope block on downloads. If above min but below file_size + min: per-file failure. Pre-check avoids partial downloads and `.partial` cleanup. | planned (revise) | 13.1 |
| R-6.4.7 | `min_free_space` configuration (default: 1 GB). Downloads scope-blocked when available space falls below this threshold. Set to 0 to disable. | planned (revise) | 13.1 |

### R-6.8 Network Resilience

| ID | Requirement | Status | Part |
|----|------------|--------|------|
| R-6.8.4 | Treat HTTP 429 as account-scoped: all drives under the same account share the throttle. | **new** | 7.4 |
| R-6.8.5 | Parse `RateLimit-Remaining` headers; proactively slow when <20% remaining. | **new** | 7.4 |
| R-6.8.6 | Honor `Retry-After` from both 429 and 503 responses. Populate `GraphError.RetryAfter` field. | **new** | 7.4 |
| R-6.8.7 | During sync, workers shall never block on retry backoff. Failures recorded in sync_failures with `next_retry_at`; reconciler re-injects when due. Workers block only for HTTP RTT (~100-500ms). | **new** (revised) | 7.2 |
| R-6.8.8 | For sync operations, graph client shall use `SyncTransport` policy (`MaxAttempts: 0`). Each dispatch = one HTTP request. CLI retains `Transport` policy (`MaxAttempts: 5`). Policy is a constructor parameter. | **new** | 7.2 |
| R-6.8.9 | The executor shall not contain a retry loop. `withRetry`, `classifyError`, `classifyStatusCode`, `sleepFunc`, and `errClass` constants removed. Executor dispatches; engine classifies and schedules. | **new** | 7.2 |
| R-6.8.10 | Transient failures recorded immediately in sync_failures with `next_retry_at`. All retry via sync_failures + reconciler. No in-memory retry budget. | **new** | 7.5 |
| R-6.8.11 | Unified non-compounding backoff: single curve via sync_failures + reconciler (1s, 2s, 4s... max 1h, ±25% jitter). One mechanism, one curve. | **new** | 7.5 |
| R-6.8.12 | Target drive identity flows through the pipeline without lookup: planner → Action → TrackedAction → WorkerResult → engine. No component queries drive identity from DB or API during failure handling. | **new** | 11.9 |
| R-6.8.13 | `Action` shall expose `TargetsOwnDrive() bool` and `ShortcutKey() string`. These are the only drive-identity accessors for scope matching and routing. Internal drive/item IDs are encapsulated. | **new** | 11.9 |
| R-6.8.14 | `SyncTransport` (`MaxAttempts: 0`) shall still perform transparent 401 token refresh inside `doOnce()`. Auth refresh is lifecycle, not transient retry. If refresh fails, return `ErrUnauthorized` (fatal). | **new** | 12.7 |
| R-6.8.15 | The engine shall classify the following HTTP status codes as transient: 5xx (`server_error`), 408 (`request_timeout`), 412 (`transient_conflict`), 404 (`transient_not_found`), 423 (`resource_locked`). Transient errors are recorded immediately in sync_failures with the specific issue_type and retried via reconciler (single curve: 1s→1h). 423 reclassified from skip to transient — non-blocking retry via reconciler handles multi-hour locks naturally. | **new** | Table 1, 7.2 |

### Design Document Changes

| Document | Changes |
|----------|---------|
| `spec/design/retry.md` | Add `SyncTransport` policy. Delete `Action` policy. Delete `CircuitBreaker` (dead code, superseded by scope-based blocking). Document unified backoff schedule. Document scope-classified retry with trial actions. |
| `spec/design/sync-execution.md` | Delete executor `withRetry`, `classifyError`, `classifyStatusCode`, `sleepFunc`, `errClass`. Executor becomes thin action dispatcher. Document TrackedAction extensions (IsTrial, TrialScopeKey). Document tracker held queue, scope gating, trial dispatch. Retry via sync_failures + reconciler (no tracker delayed queue). |
| `spec/design/sync-engine.md` | Add `classifyResult()` (single classification point, target-drive-aware). Add `updateScope()` (scope pattern detection, routes to correct scope key based on target drive). Add `WorkerResult` struct with `TargetDriveID` and `ShortcutKey`. Add `ScopeState` data structure. Add stale failure cleanup step. Add aggregated logging. Add scanner `ScanResult` contract (Events + Skipped). |
| `spec/design/sync-observation.md` | Scanner returns `ScanResult{Events, Skipped}` instead of `[]ChangeEvent`. Add `SkippedItem` struct. Scanner remains a pure observer with no DB dependency. Remove `isValidOneDriveName()` call from remote observer (observer_remote.go:405) — OneDrive enforces its own naming rules server-side; upload naming rules should not apply to downloads. |
| `spec/design/sync-store.md` | Add new issue types: `quota_exceeded`, `local_permission_denied`, `case_collision`, `disk_full`, `service_outage`. Add scope key column to `sync_failures` (e.g., `"quota:own"`, `"quota:shortcut:$drive:$item"`) for per-scope grouping in `issues` display. Add `UpsertActionableFailures` batch method. Add `ClearResolvedActionableFailures` method. |
| `spec/design/graph-client.md` | Add `SyncTransport` policy. Add `RetryAfter` field to `GraphError`. Document that sync callers use 0-retry transport. Document `RateLimit-*` header parsing. Document 401 token refresh behavior: transparent refresh inside `doOnce()`, independent of retry policy. |
| `spec/design/cli.md` | Update `issues` command for grouped display (R-2.3.7). Add per-scope sub-grouping for drive-specific issues (R-2.3.8). Add per-error-type user action text with scope-owner-specific variants (R-6.6.11, see Table 3). |
| `spec/reference/graph-api-quirks.md` | Add Microsoft throttling scope documentation (per-user, per-tenant, per-app-per-tenant limits). |

### Code Changes

| Area | Files | Changes |
|------|-------|---------|
| **Retry policies** | `internal/retry/named.go` | Add `SyncTransport` (MaxAttempts: 0). Delete `Action`. |
| **Graph client policy** | `graph/client.go` | Add `retryPolicy retry.Policy` field to `Client`. Accept as constructor parameter (default: `retry.Transport`). Replace all 4 hardcoded `retry.Transport.MaxAttempts` references in `doRetry` (lines 124, 166) and `doPreAuthRetry` (lines 318, 352) with `c.retryPolicy.MaxAttempts`. Sync callers pass `SyncTransport`. |
| **Graph client RetryAfter** | `graph/client.go`, `graph/errors.go` | Add `RetryAfter time.Duration` to `GraphError`. Parse `Retry-After` header for 429 and 503. Parse `RateLimit-Remaining` headers. |
| **Store interfaces** | `sync/store_interfaces.go`, `sync/baseline.go` | Delete `FailureRecorder` interface and `RecordFailure` method. Consolidate to `SyncFailureRecorder.RecordSyncFailure` for all callers. Move `remote_state` failure transitions from `RecordFailure` into `CommitOutcome`. |
| **Executor** | `sync/executor.go` | Delete `withRetry`, `classifyError`, `classifyStatusCode`, `sleepFunc`, `errClassRetryable/Fatal/Skip`. Action methods call graph client directly, return Outcome. |
| **Engine classification** | `sync/engine.go` | Add `classifyResult()`. Move all error classification here. |
| **Engine scope** | `sync/engine.go` | Add `ScopeState`, `updateScope()` (target-drive-aware: routes 507/403 to per-drive scope keys, 429/5xx to account/service keys), sliding window detection, scope block management. |
| **Tracker** | `sync/tracker.go` | Add `IsTrial`, `TrialScopeKey` to `TrackedAction`. Add held queue (per scope key). Add scope gating in dispatch. Add trial timer. Add `releaseScope()`, `dispatchTrial()`. No delayed queue, no ReQueue — retry handled by sync_failures + reconciler. |
| **Scanner** | `sync/scanner.go` | Return `ScanResult{Events, Skipped}` instead of `[]ChangeEvent`. Add `SkippedItem` struct. |
| **Engine observation** | `sync/engine.go` | Process `ScanResult.Skipped`: `recordSkippedItems()`, `clearResolvedActionableFailures()`. Aggregated logging. |
| **Engine permissions** | `sync/engine.go`, `permissions.go` | Add `os.ErrPermission` detection. Add parent directory check. Add directory-scope block. |
| **Store** | `sync/baseline.go` | Add `UpsertActionableFailures` (batch). Add `ClearResolvedActionableFailures`. Extend success cleanup to cover download/delete (not just upload). Add new issue types. |
| **Case collision** | `sync/scanner.go` or `upload_validation.go` | Case-insensitive name map per directory. |
| **Issues display** | `issues.go` | Grouped output for >10 items. Per-error user action text. Sub-group per-drive scope issues (507, 403) by scope key. Show drive/shortcut name and scope-owner-specific action text (R-2.3.8). |
| **Worker** | `sync/worker.go` | Extract `RetryAfter` from `GraphError` into `WorkerResult`. Populate `TargetDriveID` and `ShortcutKey` from the action, so the engine knows which quota owner and permission domain the result belongs to. |
| **Circuit breaker** | `internal/retry/circuit.go`, `circuit_test.go` | **Delete.** Dead code — never imported in production. Scope-based blocking with trial actions supersedes classic circuit breaker. Update `doc.go` to remove circuit breaker documentation. |
| **Disk space check** | `sync/executor_transfer.go` | Pre-check available disk space before download. Two-level: critical (< min_free_space → scope block) vs per-file (< file_size + min_free_space → per-file failure). |
| **Disk space scope** | `sync/engine.go` | Add `disk:local` scope key for critical disk_full. Immediate detection (single failure, deterministic). Trial at 5 min, 2× backoff, max 1 hour. |
| **Remote observer cleanup** | `sync/observer_remote.go` | Remove `isValidOneDriveName()` call (line 405). OneDrive enforces its own naming rules — upload naming rules should not reject downloads. |
| **Success clears all** | `sync/baseline.go` | Extend `CommitOutcome` to clear sync_failures for download, delete, and move successes (currently upload-only). |
| **Graph client 401** | `graph/client.go` | Add transparent token refresh on 401 inside `doOnce()`, independent of retry policy. Refresh → re-attempt once. If still 401 → `ErrUnauthorized`. |

### Test Changes

| Test | Validates |
|------|-----------|
| Graph client with `SyncTransport`: single attempt, no retry, returns raw error | R-6.8.8 |
| Graph client with `Transport`: 5 retries (unchanged behavior for CLI) | R-6.8.8 |
| 507 not retried at transport level | R-2.10.1 |
| Executor has no retry loop, returns result directly | R-6.8.9 |
| Engine `classifyResult` correctly maps all HTTP codes and error types | R-6.8.9 |
| Transient failure recorded in sync_failures with next_retry_at | R-6.8.7, R-6.8.10 |
| Reconciler re-injects from sync_failures when next_retry_at due | R-6.8.7 |
| Tracker scope block: matching actions moved to held queue | R-2.10.3 |
| Scope detection: 3 consecutive 507 from different files → account block | R-2.10.3 |
| Scope detection: 5 consecutive 5xx from different files → service block | R-2.10.3 |
| Scope detection: single 429 → immediate account block with Retry-After | R-2.10.3, R-2.10.7 |
| Trial dispatch: one action released from held queue | R-2.10.5 |
| Trial success: scope cleared, all held released | R-2.10.5, R-2.10.11 |
| Trial failure: interval doubled, action re-held | R-2.10.5, R-2.10.14 |
| Trial timing per scope type | R-2.10.14 |
| Race bound: max worker-count in-flight during scope set | R-2.10.15 |
| Failure recorded in sync_failures immediately on transient error | R-6.8.10 |
| Unified backoff: non-compounding, sequential phases | R-6.8.11 |
| Scanner returns ScanResult with Skipped items | R-2.11.5 |
| Engine records skipped items as actionable failures | R-2.11.5 |
| Engine clears resolved actionable failures | R-2.10.2 |
| Upload success clears sync_failures (existing + extend to download/delete) | Gap fix |
| Local permission denied → actionable, directory-level check | R-2.10.12 |
| Directory permission recheck clears on access | R-2.10.13 |
| Case collision detected, neither file uploaded | R-2.12.1, R-2.12.2 |
| Aggregated logging: >10 same-type → 1 summary WARN | R-6.6.7 |
| Individual retries at DEBUG, exhausted at WARN | R-6.6.8, R-6.6.10 |
| Issues display groups >10 items | R-2.3.7 |
| Per-error-type user action text present | R-6.6.11 |
| `Retry-After` parsed for 429 and 503, populates `GraphError.RetryAfter` | R-6.8.6 |
| `RateLimit-Remaining` proactive slowdown | R-6.8.5 |
| `GraphError` 400 with outage signature classified as transient | R-6.7.14 |
| 507 on own drive → `quota:own` scope; shortcut uploads continue | R-2.10.1, R-2.10.3 |
| 507 on shortcut → `quota:shortcut:$drive:$item` scope; own-drive uploads continue | R-2.10.1, R-2.10.3 |
| 429 blocks all drives including shortcuts (same token) | R-6.8.4 |
| 403 on shortcut → per-shortcut permission scope, doesn't affect other shortcuts or own drive | R-2.10.3 |
| WorkerResult carries TargetDriveID and ShortcutKey from action | R-2.10.1 |
| Issues display sub-groups 507/403 by scope (own drive vs shortcut) | R-2.3.8 |
| Quota exceeded text: "Your storage" for own drive, "Folder owner's storage" for shortcut | R-6.6.11 (Table 3) |
| Download success clears sync_failures entry (currently upload-only) | R-2.10.41 |
| Delete success clears sync_failures entry | R-2.10.41 |
| Scope detection window: success from any path resets unique-path failure counter | R-2.10.42 |
| Scope detection window: concurrent worker interleaving handled correctly | R-2.10.42 |
| Disk space pre-check: available < min_free_space → disk:local scope block | R-2.10.43 |
| Disk space pre-check: available ≥ min_free_space but < file_size → per-file failure, no scope | R-2.10.44 |
| Disk_full trial: real download attempt tests space availability | R-2.10.43 |
| Remote observer does NOT call `isValidOneDriveName()` on delta items | Cleanup |
| Circuit breaker code deleted, no imports remain | R-12.14 (cleanup) |
| SyncTransport with 401: token refresh succeeds, request retried once | R-6.8.14 |
| SyncTransport with 401: refresh fails → ErrUnauthorized returned | R-6.8.14 |
