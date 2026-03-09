# Failure Journeys at Scale

Step-by-step walkthrough of each failure scenario with 100k files, tracing the exact code path from detection through resolution. Each journey covers: how the issue manifests, what the user sees, what all workers/drives/accounts do during the failure, how it resolves, and where the gaps are.

## Research: Microsoft 429 Throttling Scope

Before the journeys, key research findings on Microsoft Graph throttling that inform the 429 analysis:

**Throttling is NOT per-drive.** It operates at three levels simultaneously:

| Level | Limit | Window |
|-------|-------|--------|
| **Per-user** | 3,000 requests | 5 min |
| **Per-user** | 50 GB ingress, 100 GB egress | 1 hour |
| **Per-tenant** (Business) | 18,750–93,750 resource units (scaled by license count) | 5 min |
| **Per-app-per-tenant** | 1,250–6,250 resource units | 1 min |
| **Per-app-per-tenant** | 1,200,000–6,000,000 resource units | 24 hours |
| **Per-app-per-tenant** | 400 GB ingress/egress | 1 hour |

**Resource unit costs (Graph API):**
- 1 RU: single item query (get item), delta with token, download file
- 2 RU: multi-item query (list children), create/update/delete/upload
- 5 RU: permission operations

**For Personal accounts:** The user effectively IS the tenant. User-level limits apply.
**For Business accounts:** Multiple users in the same org share the tenant bucket. Our single app's requests across all drives count against the same per-app-per-tenant pool.

Sources:
- [Microsoft Graph throttling guidance](https://learn.microsoft.com/en-us/graph/throttling)
- [Microsoft Graph service-specific throttling limits](https://learn.microsoft.com/en-us/graph/throttling-limits)
- [Avoid getting throttled or blocked in SharePoint Online](https://learn.microsoft.com/en-us/sharepoint/dev/general-development/how-to-avoid-getting-throttled-or-blocked-in-sharepoint-online)

---

## Journey 1: Invalid Filename

**Scenario:** User's sync directory contains 100k files with names like `CON.txt`, `file:name.doc`, `~$temp.xlsx` — characters/patterns that OneDrive rejects.

### Step-by-step code path

1. **Local observation** (`scanner.go`): The local observer walks the sync root. `isValidOneDriveName()` (scanner.go:532) is called during the **filter** stage of scanning, not during upload validation. Invalid names are filtered out of the local observation entirely — they never become `ChangeEvent`s.

   **Wait — re-read:** Actually, `isValidOneDriveName` is called in the scanner's filter logic to skip items during scanning. But it's ALSO called during pre-upload validation. Let me trace both paths:

   - **Scanner path** (scanner.go): During `walkDir`, each file is checked against filters. The scanner calls `shouldSkip()` which checks `isValidOneDriveName()` — if the name is invalid, the file is **skipped during scanning** and never enters the change pipeline at all. This is the **local observation filter**.

   - **Pre-upload validation path** (upload_validation.go:70): `validateSingleUpload()` calls `isValidOneDriveName()` on the action's basename. This is a second check applied after planning, before execution.

   The scanner filter is the **primary gate**. If a file has an invalid name, it's excluded from local observation. No `ChangeEvent` is generated, no action is planned, no validation runs. The file simply doesn't exist as far as the sync engine is concerned.

   **But:** The pre-upload validation catches a different case — what if a file with an invalid name somehow got past the scanner (e.g., the scanner filter was added later, or the file was renamed between scan and execution)?

2. **If files somehow reach planning** (e.g., the scanner filter doesn't catch them): The planner creates `ActionUpload` actions for all 100k files.

3. **Pre-upload validation** (`engine.go:1271`): `filterInvalidUploads()` calls `validateUploadActions()` which iterates all 100k actions. For each upload, `validateSingleUpload()` (upload_validation.go:70) checks:
   - `isValidOneDriveName(name)` — catches reserved names, invalid chars, `~$` prefix, `_vti_`, `.lock`, trailing dots, leading/trailing spaces
   - Path > 400 chars
   - File > 250 GB

   All 100k files fail validation. `validateUploadActions` returns `keep=[]` (empty) and `failures=[]ValidationFailure` (100k entries).

4. **Recording failures** (`engine.go:1279–1299`): For each of the 100k failures, the engine calls `e.baseline.RecordSyncFailure()` (baseline.go:1525). Each call:
   - Determines `category = "actionable"` via `isActionableIssue("invalid_filename")` → true
   - Sets `next_retry_at = NULL` (actionable issues are never auto-retried)
   - UPSERTs into `sync_failures` table

   **Performance concern:** 100k individual `INSERT ... ON CONFLICT` statements against SQLite, each in its own implicit transaction. This is O(100k) SQLite writes, which could take **minutes** on slow storage.

5. **Plan filtering** (`engine.go:1308`): `removeActionsByIndex(plan, keep)` returns an empty plan. No actions are dispatched to workers.

6. **Workers:** Never start (or start with zero actions). All 8 `transfer_workers` goroutines have nothing to do.

### What the user sees

**During `sync`:**
- 100k `WARN` log lines: `"pre-upload validation failed" path="CON.txt" issue_type="invalid_filename"`
- Sync completes quickly (no actual transfers)
- Report shows 0 succeeded, 100k from the plan were filtered

**Via `onedrive issues`:**
- 100k rows displayed, each with `category=actionable`, `issue_type=invalid_filename`
- Table output truncates error messages to 60 chars

**Via `onedrive status`:**
- Shows failure count from `sync_failures` table query

### How it resolves

**Self-healing?** No. The files have invalid names — this is permanent until the user acts.

**User action:**
1. Rename files to remove invalid characters/patterns
2. Next local observation detects the renamed files as new `ChangeModify` events
3. Pre-upload validation passes
4. Files upload successfully
5. `CommitOutcome` does NOT automatically clear the old `sync_failures` entry for the original path

**Pain point:** The old `sync_failures` entry for the original (invalid) path **persists** even after the user renames the file. The user must run `onedrive issues clear <path>` or `onedrive issues clear --all` to remove stale entries. There is no auto-detection that the file was renamed/removed. (Planned: R-2.10.2)

### Scope impact

- **Other workers for this drive:** Unaffected. Invalid filename failures are caught before execution — workers handle other (valid) actions normally.
- **Other drives:** Completely unaffected. Pre-upload validation is per-drive, per-plan.
- **Other accounts:** Completely unaffected.

### Spec vs code discrepancies

1. **Spec says** (failure-matrix.md §7.3): `~$` prefix and `_vti_` patterns are "not yet validated" (R-2.11.3 [planned]). **Code actually does validate them** — `isReservedPattern()` (scanner.go:586) checks `~$`, `_vti_`, `.lock`, and `desktop.ini`. The spec is **wrong/outdated**; the code is ahead.

2. **Spec says** (failure-matrix.md §7.4): Trailing dots and whitespace are "not yet validated" (R-2.11.4 [planned]). **Code does validate them** — `isValidOneDriveName()` (scanner.go:537–543) rejects trailing dots, trailing spaces, and leading spaces. Again, spec is outdated.

3. **Scanner filter vs validation:** The spec describes pre-upload validation as the primary gate, but the scanner filter is actually the first line of defense. Files with invalid names are filtered during local observation and never reach the planner. The pre-upload validation is a safety net for edge cases.

### Improvements

1. **Batch SQLite writes:** Recording 100k failures individually is slow. Use a single transaction for all `RecordSyncFailure` calls in `filterInvalidUploads`.
2. **Auto-clear stale failures (R-2.10.2):** When the scanner no longer sees a path (file renamed/deleted), auto-clear the corresponding `sync_failures` entry.
3. **Update spec:** R-2.11.3 and R-2.11.4 should be marked `[implemented]`.
4. **Consolidated error message:** Instead of 100k individual log lines, log a summary: "100,000 files rejected by pre-upload validation (invalid_filename: 95,000, path_too_long: 5,000)".

---

## Journey 2: OneDrive Full (HTTP 507)

**Scenario:** User's OneDrive quota is exhausted. 100k local files need to upload. Some remote changes also need to download.

### Step-by-step code path

1. **Observation:** Local observer detects 100k new/modified files. Remote observer may detect changes too. Both produce `ChangeEvent`s normally.

2. **Planning:** Planner creates 100k `ActionUpload` actions + potentially some `ActionDownload` actions.

3. **Pre-upload validation:** `filterInvalidUploads()` runs. Names are valid, sizes are fine. All 100k uploads pass validation.

4. **Execution begins:** Workers start processing the plan. 8 workers pick up actions from the tracker.

5. **First upload hits 507:** Worker calls `executeUpload()` → `e.withRetry()` → `e.transferMgr.UploadFile()` → graph client → HTTP 507 response.

6. **Graph layer** (`client.go:157–162`): `doRetry` reads the error body (64 KiB cap). `classifyStatus(507)` returns `nil` (507 has no sentinel — it falls through to the 5xx check). Actually wait — let me re-check. `classifyStatus` (errors.go:51–80) maps status codes. 507 is NOT in the explicit switch cases. It falls to the `default` — returns `nil`. Then `isRetryable(507)` is called: 507 >= 500, so `isRetryable` returns `true`. The graph client **retries 507 at the transport level** with exponential backoff (5 attempts).

   **All 5 transport retries hit 507.** The graph client returns a `*GraphError{StatusCode: 507, Err: ErrServerError}`.

7. **Executor classification** (`executor.go:290`): `classifyStatusCode(507)` hits `case http.StatusInsufficientStorage: return errClassFatal`.

8. **`withRetry` aborts** (`executor.go:310`): `classifyError` returns `errClassFatal` (not retryable). The error propagates up immediately.

   **But wait:** The error already went through 5 transport-level retries before reaching the executor. So the executor's `withRetry` doesn't retry again — it sees the fatal classification and returns immediately.

9. **Worker reports failure:** `executeUpload` returns `failedOutcome()`. The worker calls `sendResult()` with `success=false`, `HTTPStatus=507`.

10. **`processWorkerResult`** (`engine.go:933`): `r.HTTPStatus` is 507, not 403, so the permission check is skipped. `RecordFailure()` is called with `direction="upload"`.

11. **`RecordFailure`** (`baseline.go:1068`): Since direction is `"upload"`, the `remote_state` transition block is skipped (line 1078: only download/delete transition remote_state). The sync_failures UPSERT creates a row with `category='transient'`, `next_retry_at` set by exponential backoff.

    **Critical:** `RecordFailure` always sets `category='transient'`. There is no check for 507 → actionable. The failure is recorded as transient with a retry time.

12. **What about the other 99,999 uploads?** Here's the key: `errClassFatal` in `withRetry` returns the error to `executeUpload`, which returns `failedOutcome`. The worker reports the result and picks up the **next action**. `errClassFatal` does NOT abort the entire sync pass from the worker's perspective — it just means "don't retry this action within the executor." The worker continues processing other actions.

    **Wait — does errClassFatal abort the sync pass?** Let me re-check. In `withRetry` (executor.go:310), if `classifyError != errClassRetryable`, it returns the error. The caller (`executeUpload`) creates a `failedOutcome`. The worker's `executeAction` (worker.go:177) calls `dispatchAction`, gets the outcome, calls `CommitOutcome` (which is a no-op for `!outcome.Success`), then calls `sendResult`. The worker then picks up the next action from `tracker.Ready()`.

    **So: every single one of the 100k uploads goes through the same cycle:** attempt upload → 5 transport retries → 507 → errClassFatal → failedOutcome → RecordFailure(transient) → next action. Each upload wastes 5 HTTP round-trips.

13. **Downloads continue normally:** If there are download actions in the plan, workers pick them up and execute them. Downloads don't hit 507. They succeed.

14. **After all actions complete:** The SyncReport shows 100k failed uploads, N successful downloads.

### What the user sees

**During `sync`:**
- Sync takes a long time — each of the 100k uploads attempts 5 transport retries before giving up
- 100k `WARN` log lines from `withRetry`: `"retrying after transient error" operation="upload foo.txt" attempt=1 error="graph: HTTP 507: ..."`
- Actually 5 × 100k = **500k retry log lines** at the transport level
- Then 100k `WARN` log lines from `failedOutcome`: `"action failed" action="upload" path="foo.txt"`
- Downloads proceed normally amidst the upload failures

**Via `onedrive issues`:**
- 100k rows, all `category=transient`, `http_status=507`
- **No indication that this is a quota problem** — the issue_type is empty (not `quota_exceeded`)
- `issues retry` would reset them for retry, but they'd all fail again

**Via `onedrive status`:**
- Shows 100k failures

**In watch mode:**
- After the first sync pass fails for all 100k uploads, the reconciler (reconciler.go) schedules retries using `retry.Reconcile` policy (30s initial, doubling to 1h max)
- On first retry: all 100k items have `next_retry_at` ≈ now+30s. The reconciler dispatches all 100k as synthetic `ChangeEvent`s. The planner re-plans them. The executor tries again. All fail again. `RecordFailure` increments `failure_count`, computes new `next_retry_at` ≈ now+60s.
- On second retry: same thing, backoff now ≈ 2 min.
- This continues indefinitely. Each retry cycle hammers the API with 5 × 100k = 500k requests that all return 507. Backoff eventually caps at 1h between cycles, but each cycle is still a burst of 500k requests.

### How it resolves

**Self-healing?** No. Quota doesn't free itself.

**User action:** Free OneDrive space (delete files, empty recycle bin, upgrade plan). Then:
1. Next retry cycle succeeds
2. `CommitOutcome` marks items as synced
3. **But:** `CommitOutcome` does NOT delete the `sync_failures` row. The failure entry persists with `category=transient` and a `next_retry_at` in the past.

**Actually:** Looking at the code more carefully — when the reconciler dispatches a retry and the action succeeds, the flow is: `CommitOutcome` writes baseline → worker sends `success=true` → `processWorkerResult` skips failure recording (line 934: `if !r.Success`). But the old `sync_failures` row is NOT deleted.

**Wait:** Let me check if success clears the failure. In `CommitOutcome` (baseline.go:264–299), on success it writes to baseline tables but does NOT touch `sync_failures`. The reconciler's `reconcileSyncFailures` lists items via `ListSyncFailuresForRetry`, dispatches events, but doesn't clear failures on success. The old failure row remains.

**Actually:** Looking more carefully at the full flow — when the retry event re-enters the planner and executor, and succeeds, the `CommitOutcome` writes the baseline. But the `sync_failures` row must be cleared somewhere. Let me check... The reconciler synthesizes a `ChangeEvent`. This goes through the buffer → planner → executor → worker → `CommitOutcome`. On success, the worker sends `success=true`. `processWorkerResult` sees success and skips failure recording. But it kicks the retrier. The retrier's next sweep would find the item in `sync_failures` with an old `next_retry_at`, try to dispatch it again, but the baseline now shows `synced` status. The planner would see no change needed (remote matches baseline) and produce no action.

**The `sync_failures` row is orphaned.** It persists until the user manually clears it, or until a future sync pass happens to update it. This is a bug/gap.

### Scope impact

- **Other workers for this drive:** All 8 workers are busy processing the 100k failing uploads. Downloads interleaved with uploads proceed normally, but are starved for worker time. Each upload takes multiple seconds (5 transport retries with backoff), so workers are mostly occupied with failing uploads.
- **Other drives on the same account:** Completely independent — separate engine, separate workers. But the failing uploads consume the user's API quota (3,000 requests/5min), potentially causing 429s on the other drive.
- **Other accounts:** Unaffected.

### Spec vs code discrepancies

1. **Spec** (failure-matrix.md §6.1): "Currently `errClassFatal` — aborts entire sync pass including downloads." **Code reality:** `errClassFatal` does NOT abort the sync pass. It prevents the executor from retrying the individual action, but the worker moves on. All 100k uploads fail individually. Downloads proceed. The spec is misleading.

2. **Spec** (retry.md): "No escalation: Removed. Transient failures don't become permanent." **Code reality:** 507 is recorded as `category='transient'` and retried forever. It should be `category='actionable'` with `issue_type='quota_exceeded'`.

3. **Spec** (sync-store.md): "HTTP 507 classification: currently misclassified as transient." This correctly identifies the bug, but the fix is [planned].

### Improvements

1. **Classify 507 as actionable `quota_exceeded`** (R-2.10.1): Stop retrying uploads. Record in `issues`. Periodic recheck (e.g., query user's quota endpoint) to detect when space is freed.
2. **Continue downloads on 507:** Downloads should be unaffected by upload quota. Currently they proceed, but workers are starved. Improvement: on first 507, set a drive-level flag to suppress all uploads for this sync pass, freeing workers for downloads.
3. **Don't waste 5 transport retries per upload on 507:** 507 should be non-retryable at the transport level too. Currently `isRetryable(507)` returns true because 507 >= 500.
4. **Clear sync_failures on success:** When a retried item succeeds, the old failure row should be deleted.
5. **Batch detection:** After the first 507, recognize it as account-scoped. Don't attempt 99,999 more uploads.

---

## Journey 3: HTTP 429 (Rate Limiting)

**Scenario:** Syncing 100k files across one or more drives. The burst of API requests triggers Microsoft's rate limiting.

### Microsoft's throttling scope (from research above)

- **Per-user:** 3,000 requests / 5 min = ~10 requests/second sustained
- **Per-app-per-tenant:** 1,250 RU / 1 min (smallest tier). At 2 RU per upload, that's ~625 uploads/minute = ~10/second
- **Per-tenant:** 18,750 RU / 5 min = 62.5 RU/second

With 8 workers, each making requests concurrently, we can easily exceed 10 requests/second.

### Step-by-step code path

1. **Workers start uploading/downloading.** 8 workers make concurrent requests.

2. **First 429 response:** A worker's upload reaches the graph client. `doRetry` (client.go:103) executes the request. The API returns HTTP 429 with `Retry-After: 30` header.

3. **`retryBackoff`** (client.go:407–425): Detects 429, parses `Retry-After` header as 30 seconds. Sets `throttledUntil = time.Now() + 30s`. This is the **account-wide throttle gate** — but it's actually per-`Client`, and each drive has its own `Client`.

4. **Retry loop:** `doRetry` sleeps for 30 seconds (the Retry-After value), then retries. During this 30 seconds, this worker is blocked.

5. **Meanwhile, other 7 workers:** They start their next requests. Each calls `waitForThrottle()` (client.go:191–199) at the start of `doRetry`. They read `throttledUntil`, see it's 30 seconds in the future, and **all 7 workers sleep until the deadline**.

   **So: all 8 workers for this drive are blocked for 30 seconds.** This is correct behavior — the throttle gate prevents hammering.

6. **After 30 seconds:** All 8 workers wake up simultaneously and send requests. This creates a burst of 8 requests. If this re-triggers 429, another Retry-After is set, and the cycle repeats.

7. **If the request succeeds after the Retry-After wait:** The transport retry succeeds, the worker continues normally.

8. **If 429 persists after 5 transport retries:** The graph client returns a `*GraphError{StatusCode: 429, Err: ErrThrottled}`. The executor's `classifyStatusCode(429)` returns `errClassRetryable`. `withRetry` retries (up to 3 times at executor level). Each executor retry goes through 5 transport retries. Worst case: 5 × 3 = **15 HTTP requests per action**, all returning 429.

9. **If still 429 after all retries:** The action fails. `processWorkerResult` → `RecordFailure` with `category='transient'`, `http_status=429`. Reconciler will retry later with exponential backoff (30s, 60s, 120s, ... 1h).

### Multi-drive behavior

Each drive has its own `graph.Client` instance with its own `throttledUntil`. **This is the critical gap:**

- **Drive A** hits 429 with `Retry-After: 30`. Drive A's 8 workers sleep for 30s.
- **Drive B** (same user, same tenant) keeps hammering. Its `Client` has `throttledUntil` in the past. It sends requests that also get 429 (because Microsoft throttles per-user, not per-drive).
- Drive B's workers now sleep for 30s. But Drive A's workers wake up and start hammering again.
- **Ping-pong effect:** Drive A and Drive B alternate hitting 429, each blocking themselves but not the other.

### What the user sees

**During `sync`:**
- Sync slows dramatically. Workers log: `"retrying after transient error" operation="upload foo.txt" backoff=30s error="graph: HTTP 429: Too Many Requests"`
- Downloads and uploads all slow down (throttle gate blocks ALL request types for a given client)
- For a 100k file sync with 429 throttling, each action may take 30s+ instead of <1s. At 8 workers × 30s per request, throughput drops to ~16 actions/minute. 100k actions → ~100 hours.

**Via `onedrive issues`:**
- Items that exhausted all retries appear as transient failures with `http_status=429`
- Most items eventually succeed (429 is transient — it resolves once the rate window passes)

**Via `onedrive status`:**
- Shows in-progress sync, may show failure count for items that exhausted retries

### How it resolves

**Self-healing?** Yes — 429 is inherently self-healing. Once the rate limit window passes (5 minutes for user-level), requests succeed. The Retry-After header tells us exactly when.

**Resolution speed:** Depends on how aggressively we retry. If we respect Retry-After (30s is typical), the sync resumes normally after the wait. But with 8 workers all waking up simultaneously, we may immediately re-trigger 429 → sawtooth pattern.

### Spec vs code discrepancies

1. **Spec** (graph-client.md): "Per-tenant rate limit coordination: multiple drives under the same tenant share Graph API rate limits. A shared rate limiter per-tenant prevents aggregate throttling. [planned]." **Code:** Each drive has its own `Client.throttledUntil`. No sharing between drives.

2. **Pre-auth URLs bypass the throttle gate:** `doPreAuthRetry` (client.go:301) does NOT call `waitForThrottle()`. Upload chunk requests (which use pre-authenticated URLs) bypass the throttle gate entirely. They'll keep hammering even when the account is throttled. However, upload/download pre-auth URLs may have separate rate limits from the Graph API proper, so this might be intentional.

3. **429 is counted as "retryable" but also triggers the account-wide gate.** This means it's retried at both transport AND executor level, but the throttle gate prevents immediate retries. The interaction between these two mechanisms could be simplified.

4. **The spec doesn't document** that Microsoft throttles at per-user (3,000/5min), per-tenant (RU-based), AND per-app-per-tenant levels simultaneously. The reference doc (graph-api-quirks.md) doesn't mention this.

### Improvements

1. **Shared throttle gate per account** (planned in graph-client.md): When any drive's Client receives 429, propagate the `throttledUntil` deadline to all Clients for that account. This prevents Drive B from hammering while Drive A backs off.
2. **Staggered wake-up:** When the throttle gate expires, don't wake all 8 workers simultaneously. Stagger by a few hundred milliseconds to avoid a burst.
3. **Adaptive request rate:** Before hitting 429, use the `RateLimit-Remaining` header (Microsoft supports this in beta) to proactively slow down.
4. **Document throttle scope in reference docs:** Add Microsoft's actual per-user/per-tenant/per-app limits to `graph-api-quirks.md`.
5. **Pre-auth URL throttle awareness:** Consider whether upload/download pre-auth URLs should also respect the throttle gate.
6. **Reduce transport retries for 429:** The transport layer retries 429 up to 5 times. But 429 with Retry-After already sets the throttle gate. The first retry (after Retry-After) usually succeeds. 5 retries is excessive for 429 — it's more useful for 5xx errors where the next request might hit a different server.

---

## Journey 4: Local Filesystem Permission Error

**Scenario:** 100k files in the sync directory are owned by another user or have restrictive permissions (e.g., `chmod 000`). Uploads fail because the files can't be read.

### Step-by-step code path

1. **Local observation** (`scanner.go`): The scanner walks the directory via `filepath.WalkDir`. For files it can't `stat()` (permission denied), Go's `fs.WalkDirFunc` receives the error. The scanner handles walk errors by logging a warning and continuing. The file is **still added as a ChangeEvent** — the scanner reports what it sees in the directory listing (the filename), not the file contents.

   **Actually:** Let me re-check. `filepath.WalkDir` can encounter permission denied in two ways:
   - Can't read directory contents → `fs.WalkDirFunc` receives error, scanner skips the directory
   - Can stat the entry but can't read the file → the entry IS visible in the directory, scanner creates a ChangeEvent with the file's metadata (name, size from stat)

   For `chmod 000` files: `stat()` may succeed (directory permissions allow listing), so the file appears in the local observation with its metadata. A `ChangeEvent{Source: SourceLocal, Type: ChangeModify}` is generated.

2. **Planning:** The planner creates `ActionUpload` for each of the 100k files.

3. **Pre-upload validation:** All 100k pass (names are valid, sizes are fine, paths are short).

4. **Execution:** 8 workers start. Worker picks an upload action.

5. **`executeUpload`** (executor_transfer.go:82): Calls `containedPath()` (succeeds), then `e.withRetry(ctx, "upload "+action.Path, func() { e.transferMgr.UploadFile(...) })`.

6. **`UploadFile`** (`transfer_manager.go`): Opens the local file for reading. `os.Open(localPath)` returns `*os.PathError` wrapping `syscall.EACCES` (permission denied). This error propagates up.

7. **Error classification** (`executor.go:273`): `classifyError(err)` checks:
   - Is it `context.Canceled`? No.
   - Is it `*graph.GraphError`? No — it's an `*os.PathError`.
   - Is it `graph.ErrUnauthorized`? No.
   - Is it `graph.ErrThrottled` or `graph.ErrServerError`? No.
   - **Falls through to:** `return errClassSkip` (line 288).

8. **`withRetry` returns immediately** (executor.go:310): `errClassSkip` is not retryable. The error propagates to `executeUpload` → `failedOutcome`.

9. **Worker reports failure:** `sendResult` with `success=false`, `HTTPStatus=0` (no graph error).

10. **`processWorkerResult`** (engine.go:933): `r.HTTPStatus` is 0, not 403. `RecordFailure()` is called with `direction="upload"`, `httpStatus=0`.

11. **`RecordFailure`** (baseline.go:1068): Direction is upload, so remote_state transition is skipped. UPSERTs into `sync_failures` with `category='transient'`, `next_retry_at` = now + backoff.

12. **This repeats for all 100k files.** Each worker processes files one by one, each fails instantly (no network call), each recorded as transient.

13. **In watch mode:** The reconciler fires after `next_retry_at` expires (30s for first failure). All 100k items are re-dispatched. All fail again immediately. `failure_count` increments, backoff doubles. This continues forever: 30s, 60s, 2m, 4m, 8m, 16m, 32m, 1h, 1h, 1h... (capped at 1h).

### What the user sees

**During `sync`:**
- Sync completes quickly (no network calls — all failures are instant)
- 100k `WARN` log lines: `"action failed" action="upload" path="..." error="open .../file: permission denied"`
- Report: 0 succeeded, 100k failed

**Via `onedrive issues`:**
- 100k rows, all `category=transient`, `issue_type=""` (empty), `http_status=0`
- Error messages say "permission denied" but there's no specific issue type
- `issues retry --all` would reset them, but they'd all fail again immediately

**In watch mode:**
- Retries every 30s → 1min → 2min → ... → 1h
- Each retry cycle: 100k instant failures, 100k SQLite writes
- CPU spike every retry cycle as all 100k items are processed

### How it resolves

**Self-healing?** No. File permissions don't change on their own.

**User action:** `chmod` the files to be readable. Then:
1. Next retry cycle succeeds
2. **But:** Same `sync_failures` orphan issue as Journey 2 — success doesn't clear the failure row.

### Scope impact

- **Other workers:** Since permission errors fail instantly (no I/O wait), workers churn through the 100k failures very quickly. Other valid uploads/downloads proceed normally between failures. Minimal impact on throughput for valid files.
- **Other drives/accounts:** Completely unaffected (no API calls made).

### Spec vs code discrepancies

1. **Spec** (failure-matrix.md §5.3, §11.2): "Gap — classified as generic transient failure, retried forever." **Code confirms:** `errClassSkip` → `RecordFailure` with `category='transient'`. Retried forever. Spec correctly identifies this as a gap.

2. **No `os.ErrPermission` detection:** The code does `errors.Is(err, os.ErrPermission)` checks nowhere in the executor error classification. All non-graph errors fall to `errClassSkip` and then `RecordFailure` as transient.

### Improvements

1. **Detect `os.ErrPermission`:** In `classifyError`, add `errors.Is(err, os.ErrPermission)` → return a new error class or detect it in `processWorkerResult` and call `RecordSyncFailure` with `issue_type="local_permission_denied"`, `category="actionable"`.
2. **New issue type `local_permission_denied`:** Add to `isActionableIssue()`. Prevents retry. Shows in `issues` with clear message.
3. **Batch detection:** After N consecutive permission denied errors for the same directory, recognize the directory itself is unreadable and suppress all its children.
4. **Clear sync_failures on success** (same as Journey 2).

---

## Journey 5: File Too Large

**Scenario:** 100k files over 250 GB each (e.g., VM disk images) need to upload.

### Step-by-step code path

Identical to Journey 1 (Invalid Filename), except the issue type is different:

1. **Local observation:** Files are observed normally (scanner doesn't filter by size).
2. **Planning:** 100k `ActionUpload` actions created.
3. **Pre-upload validation** (upload_validation.go:91–97): `a.View.Local.Size > maxOneDriveFileSize (250 GB)` triggers. Each file gets `ValidationFailure{IssueType: IssueFileTooLarge}`.
4. **Recording:** 100k calls to `RecordSyncFailure` with `issue_type="file_too_large"`, `category="actionable"`, `next_retry_at=NULL`.
5. **Workers:** Never see these actions (filtered from plan before execution).

### What the user sees

Same as Journey 1 but with `issue_type="file_too_large"` and message "file exceeds OneDrive maximum size of 250 GB".

### How it resolves

**Self-healing?** No. User must split files, exclude them via `skip_files` config, or accept they can't sync.

**Stale failure entries:** Same orphan issue as Journey 1 — if user excludes the file via config, the `sync_failures` entry persists. Must manually `issues clear`.

### Scope impact, discrepancies, improvements

Same as Journey 1. No spec/code discrepancy for this one — `IssueFileTooLarge` is correctly implemented.

---

## Journey 6: Path Too Long

**Scenario:** 100k files with paths exceeding 400 characters (deeply nested directories).

### Step-by-step code path

Identical to Journey 1 and 5:

1. **Pre-upload validation** (upload_validation.go:82–88): `len(a.Path) > maxOneDrivePathLength (400)` triggers.
2. **Recording:** `issue_type="path_too_long"`, `category="actionable"`.
3. **Workers:** Never see these actions.

### What the user sees

Same pattern as Journey 1/5 but with "path exceeds OneDrive maximum length of 400 characters".

### Nuance: directory depth chains

If a deeply nested directory has 100k files, all at paths > 400 chars, the validation rejects them all individually. The user sees 100k separate failure entries even though the root cause is a single deeply nested directory.

### Improvements (beyond Journey 1 improvements)

1. **Group by common prefix:** In `issues` output, group failures by directory to show "directory foo/bar/baz/ and all 100k descendants: path too long" instead of 100k individual entries.
2. **Suggest `skip_dirs`:** The error message could suggest adding the problematic directory to `skip_dirs` in config.

---

## Journey 7: OneDrive Outage (HTTP 400/500-504)

**Scenario:** Microsoft backend outage. All Graph API calls return HTTP 400 "ObjectHandle is Invalid" or HTTP 500/502/503/504 for hours. 100k files pending sync.

### Step-by-step code path

Two sub-scenarios depending on the error code:

#### Sub-scenario A: HTTP 500/502/503/504 (server errors)

1. **Worker executes action.** Graph client's `doRetry` makes the request. Returns 5xx.

2. **Transport retry** (client.go): `isRetryable(500)` → true. Retries up to 5 times with exponential backoff (1s, 2s, 4s, 8s, 16s). All 5 retries fail with 5xx. Total time per action: ~31 seconds.

3. **Executor retry** (executor.go): `classifyStatusCode(500)` → `errClassRetryable`. `withRetry` retries up to 3 times. Each retry goes through 5 transport retries. Total: 3 × 5 = 15 HTTP requests, ~93 seconds per action.

4. **All retries exhausted.** `failedOutcome` → `RecordFailure` with `category='transient'`, `http_status=500`.

5. **With 8 workers and 100k actions:** Each action takes ~93 seconds. 8 workers process ~8 actions per 93 seconds = ~5 actions/minute. 100k actions → ~333 hours. In practice, the sync would run for hours making futile requests before the outage resolves.

6. **In watch mode:** After all 100k fail, reconciler schedules retries. When the outage resolves (hours later), the next retry cycle succeeds.

#### Sub-scenario B: HTTP 400 "ObjectHandle is Invalid"

1. **Transport retry** (client.go): `isRetryable(400)` → false. No transport retry. Returns immediately.

2. **Executor retry** (executor.go): `classifyStatusCode(400)` → falls to `default` → `errClassSkip`. No executor retry either.

3. **Fails instantly.** Worker processes 100k actions very quickly (no backoff). Each recorded as `category='transient'`, `http_status=400`.

4. **In watch mode:** Reconciler retries with exponential backoff (30s → 1h). Each cycle: all 100k actions dispatched, all fail instantly, all re-recorded. Hammers the API with 100k requests per cycle even during the outage.

### What the user sees

**HTTP 500:**
- Sync is extremely slow (93 seconds per action)
- Constant `WARN` logs about retries
- Eventually completes (all failed), takes hours

**HTTP 400:**
- Sync completes quickly (all failed instantly)
- 100k `WARN` log lines
- Watch mode: periodic retries (escalating backoff) with 100k requests per burst
- **No indication this is a Microsoft outage** — error messages show raw HTTP 400 + API error body

### How it resolves

**Self-healing?** Yes, eventually (minutes to hours).

**Resolution speed after outage ends:**
- HTTP 500 items have `next_retry_at` set by reconcile backoff. If many cycles have passed, backoff is at 1h max. Items won't retry for up to 1 hour after the outage resolves.
- HTTP 400 items: same backoff applies.
- User can run `issues retry --all` to force immediate retry.

### Scope impact

**HTTP 500:** All 8 workers are occupied with futile retries for ~93 seconds each. This blocks ALL actions for the drive — downloads, uploads, deletes, moves all wait in the tracker queue. Other drives are unaffected (separate workers) BUT the outage is service-wide, so they hit the same errors.

**HTTP 400:** Workers churn through failures quickly. Other actions (if any) proceed between failures. But if the outage is service-wide, everything fails.

### Spec vs code discrepancies

1. **Spec** (failure-matrix.md §3.1, R-6.7.14): "Classify as non-retryable. Report clearly that Microsoft is experiencing an outage." **Code:** HTTP 400 → `errClassSkip`, no special outage detection. The specific `ObjectHandle is Invalid` message is not parsed. All 400 errors are treated identically.

2. **Spec** (failure-matrix.md §1.8): "503 Retry-After not honored." **Code confirms:** `retryBackoff` (client.go:408) only extracts `Retry-After` for `StatusTooManyRequests` (429). If a 503 has `Retry-After`, it's ignored.

### Improvements

1. **Scope-classified retry (R-2.10.3):** Recognize service-wide failures. After N consecutive failures with the same error across different files, switch to probe mode: try ONE lightweight request (`GET /me`) periodically. Only resume full sync when the probe succeeds.
2. **Parse 400 error body for outage signatures:** If `innerError.code == "invalidRequest"` AND `message` contains "ObjectHandle is Invalid", classify specially.
3. **Honor 503 Retry-After:** Extract `Retry-After` for 503 responses, not just 429.
4. **Reduce worker waste during 500 outage:** After K consecutive 5xx failures on the same drive, pause workers for that drive and switch to probe mode.
5. **User-visible outage detection:** If all requests fail with 5xx for > 1 minute, display "Microsoft Graph API appears to be experiencing an outage" in logs and status.

---

## Journey 8: Case Collision

**Scenario:** User has 100k pairs of files that differ only in case (e.g., `Report.txt` and `report.txt` in the same directory). Each pair represents a case collision on the case-insensitive OneDrive.

### Step-by-step code path

1. **Local observation:** Scanner walks the directory. Both `Report.txt` and `report.txt` are valid local files (Linux/macOS allows this). Both generate `ChangeEvent{Source: SourceLocal, Type: ChangeModify}`.

2. **Planning:** The planner creates `ActionUpload` for both `Report.txt` and `report.txt`.

3. **Pre-upload validation** (upload_validation.go): `validateSingleUpload` checks name validity, path length, file size. **It does NOT check for case collisions.** Both actions pass validation.

4. **Execution:** Workers upload both files.

5. **First upload (`Report.txt`):** Succeeds. OneDrive creates the file.

6. **Second upload (`report.txt`):** OneDrive is case-insensitive. The API **silently overwrites** `Report.txt` with `report.txt`'s content. No error returned — HTTP 200. The upload "succeeds."

   **Wait:** Actually, the behavior depends on how the upload works:
   - If uploading by path (name + parentID): the API matches case-insensitively. `report.txt` overwrites `Report.txt`.
   - The item ID changes (new item created, old one effectively replaced).

7. **Baseline state:** Both `Report.txt` and `report.txt` have baseline entries. But OneDrive only has one file. On the next remote delta, only one item appears (with the casing of the last upload). The planner sees the "missing" file and creates a download action.

8. **Infinite loop potential:** Remote delta shows `report.txt`. Local has both `Report.txt` and `report.txt`. Planner sees `Report.txt` not in remote → plans upload. Upload overwrites `report.txt` on OneDrive. Next delta shows `Report.txt`. Planner sees `report.txt` not in remote → plans upload. **Infinite upload cycle.**

### What the user sees

**During `sync`:**
- Sync completes "successfully" — no errors visible
- On next sync: same files re-upload
- **Data loss:** One version silently overwrites the other. The user never knows.

**Via `onedrive issues`:**
- Nothing. No failures, no conflicts. The overwrite is invisible.

**Via `onedrive status`:**
- Shows normal synced state

**In watch mode:**
- Continuous re-syncing of the colliding files. Each sync cycle uploads one version, which overwrites the other.

### How it resolves

**Self-healing?** No. The collision is permanent.

**User action:** Rename one of the colliding files. But the user has NO signal that there's a problem — no error, no warning, no issue entry.

### Scope impact

- **Other workers:** The upload loop consumes worker time but doesn't block other actions. Each upload is a real network round-trip.
- **Other drives/accounts:** API quota consumed by futile re-uploads. 100k colliding pairs = 200k uploads per sync cycle.

### Spec vs code discrepancies

1. **Spec** (R-2.12.1): "Before uploading, the system shall detect local case-insensitive filename collisions and flag them as conflicts rather than attempting upload. [planned]" **Code:** No detection exists. R-2.12.1 is correctly identified as planned.

2. **Spec** (failure-matrix.md §7.7): "Silent data loss on case-insensitive OneDrive." **Code confirms:** No case collision detection anywhere in the pipeline.

### Improvements

1. **Pre-upload case collision detection (R-2.12.1):** During local observation or pre-upload validation, build a case-insensitive map of filenames per directory. Flag collisions as actionable issues before upload.
2. **New issue type `case_collision`:** Record both paths. Show in `issues` with clear message: "files 'Report.txt' and 'report.txt' collide on OneDrive (case-insensitive)."
3. **Prevent the infinite loop:** Even without pre-detection, the planner could detect that a file was uploaded in the last cycle and remote shows a different-cased version — this is a signal of a collision.
4. **Data loss priority:** This is arguably **higher priority than medium** because it causes silent data loss, unlike other failures which are at least visible.

---

## Cross-Cutting Gaps

### Gap: sync_failures rows are never cleaned up on success

Across Journeys 2, 4, and 7: when a retried action eventually succeeds, `CommitOutcome` writes the baseline but does NOT delete the `sync_failures` row. The orphaned row persists forever. The user sees "failures" in `issues` for files that are actually synced.

**Fix:** In `processWorkerResult`, when `r.Success == true`, check if a `sync_failures` row exists for the path and delete it.

### Gap: No scope-aware retry

Across Journeys 2, 3, 7: each of the 100k items is retried independently. For account-scoped failures (507, 429) and service-scoped failures (outage), this means 100k individual retries when a single probe would suffice.

**Fix:** R-2.10.3 scope-classified retry. Detect failure scope from the error type. File-scoped → individual retry. Drive/account/service-scoped → single probe, resume all on success.

### Gap: No batch SQLite writes for bulk failures

Across Journeys 1, 4, 5, 6: recording 100k failures individually is O(100k) SQLite transactions. Should batch into a single transaction.

### Gap: Stale sync_failures after user resolution

Across Journeys 1, 5, 6: when the user renames/deletes/excludes a problematic file, the old `sync_failures` entry persists. R-2.10.2 (auto-resolution detection) is planned but not implemented.

### Gap: No consolidated error reporting

All journeys produce up to 100k individual log lines. Should consolidate into summaries: "100,000 uploads failed: 95,000 permission denied, 5,000 HTTP 507".
