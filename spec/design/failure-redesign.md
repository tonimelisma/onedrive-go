# Failure Handling Redesign — Analysis & Recommendations

Comprehensive analysis addressing all questions, corrections from code review, three-alternative designs for each architectural decision, to-be journeys, and full requirements inventory.

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

Example:
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

---

## Part 4: Auto-Clearing Stale Failures — Three Designs

### Design A: Scanner-Driven Cleanup

**How:** During local observation, after the scanner completes its walk, compare the set of observed paths against `sync_failures` entries. Any `sync_failures` path not in the observed set (for upload/local-origin failures) is deleted.

**Where in the pipeline:** End of local observation phase, before planning.

**Pros:**
- Natural fit: the scanner is the source of truth for local state
- Runs every sync pass
- Simple: one SQL query (`DELETE FROM sync_failures WHERE direction='upload' AND path NOT IN (...)`)

**Cons:**
- 100k-file set comparison could be expensive (large IN clause)
- Scanner may not observe a file for legitimate reasons (filter changes, temp exclusion)
- Couples scanner to sync_failures table (scanner currently has no DB dependency)

### Design B: Planner-Driven Cleanup

**How:** During planning, after changes are computed, the planner identifies `sync_failures` paths that have no corresponding action AND no corresponding local file. These are auto-cleared.

**Where in the pipeline:** After planning, before execution.

**Pros:**
- Planner already has access to both local state and baseline
- More precise: only clears failures for truly-gone files, not just un-observed ones

**Cons:**
- Planner is supposed to be pure (no I/O, no side effects). Adding DB cleanup violates this principle.
- Would need a post-planning step in the engine, not the planner itself

### Design C: Observation-Triggered Cleanup in the Engine

**How:** When the local observer reports a `ChangeDelete` event for a path, or when the scanner completes and a path is absent from the observed set, the engine checks for and clears any `sync_failures` entry for that path. This is done in the engine's event processing, not the scanner or planner.

**Where in the pipeline:** In the engine, after observation events are collected, as a dedicated "stale failure cleanup" step.

**Pros:**
- Engine already orchestrates the pipeline — adding a cleanup step is natural
- Doesn't couple scanner to DB
- Doesn't violate planner purity
- Event-driven: delete events naturally trigger cleanup
- Can handle both "file deleted" and "file renamed" cases

**Cons:**
- "File renamed" is harder to detect — it appears as delete + create, and the create has a different path. The old failure entry needs the delete event to trigger cleanup.
- Adds a step to the engine's RunOnce flow

### Recommendation: Design C (Engine-Triggered Cleanup)

Most consistent with CLAUDE.md Engineering Philosophy:
- **"Prefer large, long-term solutions"**: Works for all failure types, not just upload validation
- **"Architecture should be extremely robust"**: Doesn't couple unrelated packages
- **"Modules can be rethought"**: Keeps scanner as a leaf observer, planner as pure, engine as orchestrator
- **"Functions do one thing"**: Cleanup is a discrete step in the engine, not mixed into scanning or planning

Implementation: Add a method `SyncStore.ClearStaleUploadFailures(ctx, observedLocalPaths)` called from the engine after local observation completes. For remote-origin failures (download/delete), clear when the reconciler dispatches a retry that succeeds (already partially done for uploads).

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

### Sub-requirements per error type

| ID | Error Type | Reason Text | User Action Text |
|----|-----------|-------------|-----------------|
| R-6.6.11.1 | `invalid_filename` | "File name '{name}' contains characters not allowed on OneDrive ({chars})" or "File name '{name}' is reserved on OneDrive" | "Rename the file to remove invalid characters, or add it to skip_files in config" |
| R-6.6.11.2 | `path_too_long` | "Path exceeds OneDrive's 400-character limit ({len} characters)" | "Shorten the directory structure or file name" |
| R-6.6.11.3 | `file_too_large` | "File exceeds OneDrive's 250 GB size limit ({size})" | "Exclude via skip_files or max_file_size in config" |
| R-6.6.11.4 | `permission_denied` | "OneDrive folder '{boundary}' is read-only (shared without write access)" | "Request write access from the folder owner, or accept read-only sync" |
| R-6.6.11.5 | `local_permission_denied` | "Cannot read file (local permission denied)" | "Fix file permissions: chmod/chown the file or its parent directory" |
| R-6.6.11.6 | `quota_exceeded` | "OneDrive storage is full ({used}/{total})" | "Free space on OneDrive (delete files, empty recycle bin) or upgrade storage plan" |
| R-6.6.11.7 | `disk_full` | "Local disk is full ({available} remaining)" | "Free local disk space" |
| R-6.6.11.8 | `case_collision` | "Files '{name1}' and '{name2}' differ only in case — OneDrive is case-insensitive" | "Rename one of the files to avoid the collision" |
| R-6.6.11.9 | `rate_limited` | "OneDrive API rate limit reached (retry in {seconds}s)" | "No action needed — will auto-resolve. Reduce transfer_workers to lower request rate" |
| R-6.6.11.10 | `service_outage` | "Microsoft Graph API appears unavailable (HTTP {code})" | "No action needed — will auto-resolve when service recovers" |
| R-6.6.11.11 | `auth_expired` | "Authentication has expired" | "Run 'onedrive login' to re-authenticate" |

---

## Part 7: Scope-Specific Retry — Three Designs

### The problem

Currently all failures retry independently. If Microsoft is down, 100k items each independently try 15 HTTP requests (5 transport × 3 executor), then independently schedule reconciler retries. No component recognizes "the service is down for everyone."

### Design A: Centralized Scope Coordinator (new component)

**Architecture:** A new `ScopeCoordinator` component sits between the executor and the graph client. It tracks failure patterns and manages scope state.

```
Workers → Executor → ScopeCoordinator → Graph Client
                          ↓
                    Scope State:
                    - file_scoped: {path → backoff}
                    - drive_scoped: {driveID → blocked_until}
                    - account_scoped: {accountID → blocked_until}
                    - service_scoped: {blocked_until}
```

**How it works:**
1. Every error passes through `ScopeCoordinator.Classify(err, path, driveID)`.
2. The coordinator maintains sliding windows of errors per scope.
3. When N consecutive errors of the same type hit different files within T seconds, it **escalates** to the next scope:
   - 5 consecutive 507s on different files → account-scoped quota issue
   - 10 consecutive 5xx on different files and drives → service-scoped outage
4. When a scope is blocked, the coordinator prevents all actions at that scope from being dispatched. A single probe request is sent periodically to detect recovery.
5. When the probe succeeds, the scope is unblocked and all pending items are immediately re-queued.

**Scope hierarchy:**
```
service (all accounts) → account → drive → file
```

**Pros:**
- Clean separation of concerns: executor doesn't need to know about scope
- Centralized state makes it easy to query (e.g., for `status` command)
- Probe-based recovery is efficient (1 request instead of 100k)
- Natural place for future 429/RateLimit header coordination

**Cons:**
- New component adds complexity
- Cross-drive and cross-account coordination requires shared state
- The coordinator needs access to all drives/accounts — architectural coupling

### Design B: Worker-Local Scope Detection with Drive-Level Propagation

**Architecture:** Each worker detects scope from consecutive errors. Drive-level state is propagated via the existing engine. No new component.

**How it works:**
1. The executor's `withRetry` tracks consecutive errors. If the same error class occurs N times in a row, it sets a drive-level flag.
2. Drive-level flags are stored in the engine: `engine.scopeFlags` map. When a flag is set (e.g., `quota_exceeded`, `service_down`), the tracker stops dispatching new actions of the affected type.
3. A probe goroutine per drive tests recovery periodically.
4. Account-level and service-level scope: the `orchestrator` (which manages multiple drives) aggregates drive-level flags. If all drives report the same scope, it's escalated.

**Pros:**
- No new component — uses existing engine/orchestrator hierarchy
- Incremental: can implement drive-scoped first, then account/service later
- Workers detect scope naturally from their own errors

**Cons:**
- Distributed detection: workers may disagree on scope
- Account-level coordination requires orchestrator changes
- Flags need careful lifecycle management (when to set, when to clear)

### Design C: Event-Driven Scope Escalation via Error Events

**Architecture:** Workers emit typed error events to a channel. A scope classifier goroutine aggregates events and makes scope decisions. The tracker reads scope state to gate dispatching.

```
Worker → errChan → ScopeClassifier → tracker.SetScopeBlock()
                                    → probeScheduler.StartProbe()
```

**How it works:**
1. When a worker encounters an error, it emits a `ScopeEvent{Error, Path, DriveID, HTTPStatus, Timestamp}` to a buffered channel (in addition to the normal WorkerResult).
2. The `ScopeClassifier` goroutine reads from this channel. It maintains a sliding window per scope:
   - Counts errors by (type, scope) in the last T seconds
   - Escalation thresholds: 3 consecutive same-type errors within 10s → drive block, etc.
3. When a scope is blocked, the classifier tells the tracker to stop dispatching actions for that scope.
4. A probe scheduler sends periodic lightweight requests (`GET /me` for service, `GET /drives/{id}/root` for drive). On probe success, the scope is unblocked and the tracker immediately releases all held actions.

**Pros:**
- Decoupled: workers don't block, classifier is async
- Event-driven: consistent with the sync pipeline's event-driven architecture
- Natural extension: can add new scope classifiers without changing workers
- The probe scheduler is a clean, testable component

**Cons:**
- Channel-based coordination adds complexity
- Potential race between classifier decision and worker dispatch
- Need to handle channel overflow for bursty error scenarios

### Recommendation: Design C (Event-Driven Scope Escalation)

Most consistent with CLAUDE.md Engineering Philosophy:
- **"Prefer large, long-term solutions"**: Generalizes to any failure scope
- **"Event-driven sync pipeline"** (system.md): This extends the existing event-driven paradigm
- **"Functions do one thing"**: Classifier, probe scheduler, and tracker each have a single responsibility
- **"Accept interfaces / return structs"**: ScopeClassifier can accept interface for testing
- **"Architecture should be extremely robust"**: Workers never block on scope decisions; classifier is async

---

## Part 8: Immediate Retry on Scope Clearance

When the root cause clears (quota freed, outage resolves, permissions change), all blocked items should retry immediately — not wait for exponential backoff to tick.

**Proposed requirements:**

**R-2.10.5:** When a scope-level block clears (detected by a successful probe), the system shall immediately re-queue all items blocked at that scope, bypassing their individual `next_retry_at` backoff timers.

**R-2.10.6:** For `quota_exceeded`: when the system detects available quota (via periodic quota endpoint query or successful upload probe), it shall immediately re-queue all suppressed uploads.

**R-2.10.7:** For `rate_limited` (429): when the `Retry-After` deadline expires, the system shall immediately release the throttle gate. (Already implemented via `waitForThrottle`.)

**R-2.10.8:** For `service_outage`: when a probe request succeeds, the system shall immediately re-queue all items blocked at the service scope.

**R-2.10.9:** For `permission_denied`: when `recheckPermissions()` discovers a previously-denied folder is now writable, the system shall immediately re-queue all items under that folder. (Partially implemented — `recheckPermissions` clears the issue but doesn't force immediate re-queue.)

**R-2.10.10:** For `local_permission_denied`: when the scanner observes a previously-blocked file is now readable, the system shall clear the failure and re-queue.

**R-2.10.11:** When scope clearance triggers re-queuing, the `failure_count` shall be preserved but `next_retry_at` shall be set to now (immediate). This ensures backoff resumes from the previous level if the scope blocks again.

---

## Part 9: 429 Scope Discovery

### Can we determine the throttle scope from the 429 response?

**Partially.** Microsoft's response doesn't explicitly say "you hit the per-user limit" vs "per-tenant limit." However:

1. **`RateLimit-*` headers (beta):** Microsoft returns `RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset` when an app consumes ≥80% of its **per-app 1-minute resource unit** limit. These headers only cover one specific limit. If throttled by a different limit (per-user, per-tenant), no `RateLimit` headers are sent.

2. **`Retry-After` value heuristic:** Short `Retry-After` (1–10s) typically indicates per-user or per-app-per-tenant limits. Long `Retry-After` (30–120s) may indicate broader throttling. This is NOT documented and should not be relied upon.

3. **Contextual inference:** If only one drive is throttled, it's likely per-user. If all drives across an account are throttled, it's likely per-tenant. But we can only infer this after the fact.

### Recommendation

**Pragmatic approach:** Treat all 429s as account-scoped. Since our app syncs on behalf of a single user (personal or business), and Microsoft's limits apply per-user (3,000 req/5min), the per-user limit is the most likely to trigger. Sharing the throttle gate across all drives for the same account is the right default.

For future refinement: parse `RateLimit-Remaining` headers when present. If they indicate we're at 80%+ of the per-app limit, proactively slow down before hitting 429.

**Proposed requirements:**

**R-6.8.4:** The system shall treat HTTP 429 as account-scoped: when any drive receives 429, all drives under the same account shall respect the `Retry-After` deadline.

**R-6.8.5:** The system shall parse `RateLimit-Remaining` headers (when present) and proactively reduce request rate when remaining quota falls below 20%.

**R-6.8.6:** The system shall extract and honor `Retry-After` headers from both 429 and 503 responses.

---

## Part 10: Worker Blocking — Three Designs

### The problem

Currently, when a worker encounters a retryable error, it sleeps inside `withRetry` (executor.go:317): `e.sleepFunc(ctx, backoff)`. During this sleep, the worker goroutine is blocked and cannot process other actions. With 8 workers and a 30s Retry-After, ALL 8 workers can be blocked simultaneously, halting all progress.

### Design A: Return-to-Tracker with Delayed Re-Queue

**How:** When `withRetry` encounters a retryable error, instead of sleeping, it returns a special "retry later" outcome. The worker completes immediately. The tracker re-queues the action with a `notBefore` timestamp.

```go
type TrackedAction struct {
    // ... existing fields
    NotBefore time.Time // don't dispatch until this time
    Attempt   int       // current retry attempt
}
```

The tracker's `Ready()` channel only dispatches actions whose `NotBefore` has passed. A timer wakes the tracker when the earliest `NotBefore` arrives.

**Pros:**
- Workers never block — maximum throughput for actions that CAN succeed
- Clean separation: retry timing is the tracker's responsibility
- Natural fit for scope-based blocking (tracker knows about all actions)
- Testable: tracker's timing behavior is deterministic with injected clock

**Cons:**
- Tracker becomes more complex (priority queue with timestamps)
- Action state management: retry attempt count must be tracked
- Memory: 100k pending actions with `NotBefore` timestamps in the tracker

### Design B: Separate Retry Queue with Dedicated Timer

**How:** Failed retryable actions go to a `RetryQueue` (a priority queue ordered by retry time). A single goroutine monitors the queue and re-injects actions into the tracker when their backoff expires. Workers are completely unaware of retries.

```
Worker → fails → RetryQueue.Enqueue(action, backoff)
                      ↓ (timer fires)
                 Tracker.ReAdd(action)
                      ↓
                 Worker picks up again
```

**Pros:**
- Workers never block
- Retry logic fully decoupled from execution
- RetryQueue can implement scope-aware batching (don't re-inject if scope is blocked)
- Simple worker code: try once, report result, move on

**Cons:**
- New component (RetryQueue)
- Action bounces between tracker → worker → retryQueue → tracker (extra indirection)
- Retry count tracking needs careful handling across the bouncing

### Design C: Async Backoff with Context-Based Wake

**How:** `withRetry` still lives in the executor, but instead of blocking via `sleepFunc`, it uses a non-blocking timer and returns the action to the worker pool's internal ready queue. The worker picks up the next available action while the timer is pending.

```go
func (e *Executor) withRetry(ctx, desc, fn) error {
    for attempt := range maxRetries {
        err := fn()
        if err == nil { return nil }
        if classifyError(err) != errClassRetryable { return err }
        // Don't sleep — return retryable error with backoff hint
        return &RetryableError{Err: err, BackoffHint: calcExecBackoff(attempt)}
    }
}
```

The worker detects `*RetryableError`, tells the tracker to re-queue with a delay, and immediately picks up the next ready action.

**Pros:**
- Minimal change to existing architecture
- Workers still execute actions; retry scheduling moves to tracker
- `withRetry` becomes simpler (no sleep)
- Compatible with scope-based gating (tracker can check scope before re-queuing)

**Cons:**
- The `RetryableError` return type changes the executor contract
- Executor retries at transport level (inside graph client) still block — this only fixes executor-level retries
- Transport-level sleeps (inside `doRetry`) still block the worker during the graph client's internal retries

### Recommendation: Design A (Return-to-Tracker with Delayed Re-Queue)

Most consistent with CLAUDE.md:
- **"Prefer large, long-term solutions"**: Fundamentally fixes worker blocking at all levels
- **"Architecture should be extremely robust"**: Workers never waste time sleeping
- **"Functions do one thing"**: Worker executes. Tracker schedules. Clean separation.
- **"No code is sacred"**: Willing to change the tracker's dispatch model

**However:** Transport-level retries (inside `graph.Client.doRetry`) also block. To fully solve this, the graph client would need to return retryable errors instead of sleeping internally. This is a larger change. Recommendation: implement Design A for executor-level retries first, then evaluate whether transport-level retries need the same treatment.

**Proposed requirement:**

**R-6.8.7:** Individual workers shall not block on retry backoff. When a retryable error occurs, the action shall be returned to the scheduling queue with a `notBefore` timestamp, and the worker shall immediately pick up the next available action.

**R-6.8.8:** The transport-level retry loop (graph client) may block for short durations (up to `Retry-After` or exponential backoff), but the executor-level retry shall not compound this by adding additional blocking delays.

---

## Part 11: Local Permission Hierarchical Checking

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

## Part 12: CLAUDE.md Engineering Philosophy Iteration

Proposed additions to the Eng Philosophy section, based on lessons from this analysis:

```markdown
## Eng Philosophy

(existing bullets unchanged)

### Failure Handling Principles

- Every user-impacting condition must be visible in `issues`. Silent filtering is a bug.
- Scope awareness: recognize whether a failure affects one file, one drive, one account, or the service. Don't retry 100k items individually when one probe would suffice.
- Workers are precious: never block a worker on a backoff timer. Return the action to the scheduler and pick up the next ready item.
- Actionable vs transient: errors that require user action (permissions, naming, quota) must never be retried automatically. Errors that self-heal (network, 5xx, 429) must never require user action.
- Show the user what's wrong AND what to do about it. Every warning and issue entry includes a plain-language reason and a concrete next step.
- Aggregate, don't spam: when the same condition affects many files, show one summary, not N identical warnings.
```

---

## Part 13: To-Be Journeys

### Journey 1 (TO-BE): Invalid Filename — 100k files

1. **Scanner** (scanner.go:271): Files fail `isValidOneDriveName`. **NEW:** Instead of only DEBUG-logging, the scanner records each invalid file in a batch collector.

2. **Post-scan recording:** The engine calls `RecordScannerSkips(ctx, skippedPaths)` which batch-inserts `sync_failures` entries with `category='actionable'`, `issue_type='invalid_filename'` in a **single transaction**.

3. **Aggregated logging (R-6.6.7):** One WARN line: `"100,000 files skipped: invalid OneDrive names"`. Individual paths at DEBUG.

4. **Upload validation** (safety net): If any somehow reach the planner, `filterInvalidUploads` catches them. Same batch recording.

5. **What the user sees:**
   - `onedrive sync`: 1 summary WARN line
   - `onedrive issues`: Grouped display: `"invalid_filename (100,000 items): CON.txt, PRN.doc, ... and 99,995 more. ACTION: Rename files or add to skip_files."` (R-6.6.11.1)

6. **Resolution:** User renames files. **NEW (R-2.10.2, Design C):** Next scanner pass doesn't see the old paths → engine's stale failure cleanup deletes the `sync_failures` entries automatically. No manual `issues clear` needed.

7. **Scope:** File-scoped. No impact on other workers, drives, or accounts.

### Journey 2 (TO-BE): OneDrive Full (507) — 100k uploads

1. **First upload hits 507.** Transport layer: `isRetryable(507)` **changed to false** (507 is not a transient server error — it's a quota condition). No transport retries.

2. **Executor classification:** `classifyStatusCode(507)` returns **new class `errClassActionable`** (not fatal, not retryable, not skip — it's a recognized user-actionable condition).

3. **Scope escalation (Design C):** Worker emits `ScopeEvent{HTTP507, path, driveID}`. Scope classifier receives it. After 3 consecutive 507s on different files within 10 seconds → **account-scoped `quota_exceeded` block**. Classifier tells the tracker to stop dispatching ALL uploads for this account.

4. **Remaining 99,997 uploads:** Never attempted. Tracker holds them. Workers are free for other actions.

5. **Downloads continue normally.** The scope block only affects uploads. Downloads, deletes, and moves proceed.

6. **Recording:** One `sync_failures` entry with `issue_type='quota_exceeded'` at account scope. Not 100k individual entries.

7. **Probe:** A periodic probe checks quota via `GET /me/drive` (returns `quota.remaining`). When quota is available → scope unblocked → **all held uploads immediately re-queued (R-2.10.6)** → workers process them.

8. **What the user sees:**
   - `onedrive sync`: 1 WARN: `"OneDrive storage full — uploads paused. Free space to resume. (100,000 uploads pending)"` (R-6.6.11.6)
   - `onedrive issues`: `"quota_exceeded: OneDrive storage full (15.0 GB / 15.0 GB). ACTION: Free space or upgrade plan. 100,000 uploads pending."`
   - `onedrive status`: Shows "uploads paused (quota)" per drive

9. **Resolution:** User frees space. Probe detects. All 100k uploads immediately process (no exponential backoff). If some fail again (quota re-filled partially), scope re-blocks.

### Journey 3 (TO-BE): 429 Rate Limiting — 100k files

1. **First 429 response.** Graph client parses `Retry-After: 30` from header. Sets `throttledUntil` on the client. **NEW (R-6.8.4):** Propagates to ALL clients for this account via shared `AccountThrottle` struct.

2. **All workers for ALL drives under this account** call `waitForThrottle()` and see the shared deadline. All wait 30 seconds.

3. **Workers are NOT blocked (R-6.8.7):** Instead of sleeping, the executor returns the action to the tracker with `NotBefore = now + 30s`. Worker immediately picks up the next ready action. If no actions are ready (all gated), the worker idles.

4. **Staggered wake-up:** When `NotBefore` expires, the tracker releases actions gradually (not all 100k at once) — e.g., 8 at a time matching worker count.

5. **RateLimit headers (R-6.8.5):** If `RateLimit-Remaining: 120` is received, and we know `RateLimit-Limit: 1200`, we proactively slow request rate before hitting 429.

6. **Logging (R-6.6.8, R-6.6.9):** Individual 429 retries: DEBUG. Throttle gate activation: INFO: `"API rate limit reached, pausing requests for 30s (affects all drives)"`. Resolution: INFO: `"Rate limit cleared, resuming"`.

7. **What the user sees:**
   - No WARN spam
   - 1 INFO line when throttled, 1 INFO line when cleared
   - Sync progresses steadily after the pause

### Journey 4 (TO-BE): Local Permission Denied — 100k files

1. **Upload fails with `os.ErrPermission`.** **NEW:** Executor detects `errors.Is(err, os.ErrPermission)`.

2. **Hierarchical check (R-2.10.12):** Engine checks if the parent directory is readable. If the directory `/data/protected/` is itself unreadable, record ONE `local_permission_denied` issue at the directory level. Suppress all 100k uploads under that directory.

3. **If directory IS readable** (individual file permissions): Record file-level `local_permission_denied` with `category='actionable'`. **Aggregate (R-6.6.7):** 1 WARN: `"100,000 files unreadable (permission denied)"`. Individual paths at DEBUG.

4. **No retry.** Actionable issues are not auto-retried.

5. **Recheck (R-2.10.13):** Each sync pass, re-check directory-level permission issues. If the directory becomes readable → auto-clear → immediate re-queue of all children (R-2.10.10).

6. **What the user sees:**
   - `onedrive issues`: `"local_permission_denied: Directory /data/protected/ is not readable. ACTION: Fix permissions with chmod/chown. (100,000 files affected)"` (R-6.6.11.5)

### Journey 5/6 (TO-BE): File Too Large / Path Too Long

Same as To-Be Journey 1 but with respective issue types. Key improvements over as-is:
- **Aggregated display in issues** (R-2.3.7)
- **Stale failure auto-cleanup** when user excludes files via config
- **Path too long grouping:** Issues grouped by common directory prefix

### Journey 7 (TO-BE): OneDrive Outage — 100k files

1. **First request returns 500/502/503.** Transport retries: **reduced to 3** for 5xx (was 5). Total: ~7 seconds per action.

2. **Scope escalation:** After 5 consecutive 5xx errors on different files within 30 seconds → **service-scoped block**. Workers stop dispatching ALL actions across ALL accounts.

3. **Probe:** Single lightweight request (`GET /me`) every 60 seconds. When probe returns 200 → service scope unblocked → all items immediately re-queued (R-2.10.8).

4. **HTTP 400 "ObjectHandle is Invalid":** **NEW (R-6.7.14):** Parse error body. If `innerError.code == "invalidRequest"` and message matches outage signature → classify as service-scoped outage, not generic skip.

5. **503 Retry-After (R-6.8.6):** If 503 has `Retry-After`, honor it (same as 429).

6. **What the user sees:**
   - 1 WARN: `"Microsoft Graph API unavailable (HTTP 502). Sync paused, probing every 60s. (100,000 actions pending)"` (R-6.6.11.10)
   - When resolved: INFO: `"Microsoft Graph API recovered. Resuming sync."`

### Journey 8 (TO-BE): Case Collision — 100k pairs

1. **Pre-upload detection (R-2.12.1):** During local observation OR pre-upload validation, build a case-insensitive map of names per directory. When two paths differ only in case → flag both as `case_collision`.

2. **Recording:** Actionable issue with `issue_type='case_collision'`. Both paths recorded. Neither uploaded.

3. **What the user sees:**
   - `onedrive issues`: `"case_collision (100,000 pairs): 'Report.txt' ↔ 'report.txt' in dir/. ACTION: Rename one file in each pair."` (R-6.6.11.8)

4. **No silent data loss.** Neither file is uploaded until the collision is resolved.

5. **Auto-clear (R-2.10.2):** When the user renames one file, the collision no longer exists. Next scan clears the issue and both files proceed normally.

---

## Part 14: Complete Requirements Inventory

All proposed new/changed requirements, organized by area. Each references the analysis that motivated it.

### R-2 Sync — Failure Management (additions to R-2.10)

| ID | Requirement | Status |
|----|------------|--------|
| R-2.10.1 | HTTP 507 → actionable `quota_exceeded`, suppress uploads only, continue downloads | planned (revise) |
| R-2.10.2 | Auto-detect file removal/rename and clear stale `sync_failures` entries | planned (revise: specify engine-driven cleanup per Design C) |
| R-2.10.3 | Scope-classified retry: file, drive, account, service scopes with per-scope backoff | planned (revise: specify event-driven scope escalation per Design C) |
| R-2.10.4 | Status display shows failure scope context alongside retry information | planned (unchanged) |
| R-2.10.5 | When a scope block clears, immediately re-queue all blocked items (bypass individual backoff) | **new** |
| R-2.10.6 | Quota clearance: probe via quota endpoint, immediate re-queue of suppressed uploads | **new** |
| R-2.10.7 | Rate limit clearance: release throttle gate when Retry-After expires | implemented (already done via `waitForThrottle`) |
| R-2.10.8 | Service outage clearance: probe-based detection, immediate re-queue | **new** |
| R-2.10.9 | Permission change: `recheckPermissions` triggers immediate re-queue of affected items | **new** (enhance existing) |
| R-2.10.10 | Local permission clearance: scanner detects readable → clear failure, re-queue | **new** |
| R-2.10.11 | Scope clearance preserves `failure_count` but sets `next_retry_at` to now | **new** |
| R-2.10.12 | Local permission denied: check parent directory accessibility, record at directory level if applicable | **new** |
| R-2.10.13 | Recheck `local_permission_denied` directory issues each sync pass, auto-clear when accessible | **new** |

### R-2 Sync — Issues Display (additions to R-2.3)

| ID | Requirement | Status |
|----|------------|--------|
| R-2.3.7 | Group issues by type when count > 10, show summary + first 5 paths, `--verbose` for all | **new** |

### R-2 Sync — Filename Validation (updates to R-2.11)

| ID | Requirement | Status |
|----|------------|--------|
| R-2.11.1 | Reject invalid characters before upload | implemented (update status) |
| R-2.11.2 | Reject reserved names | implemented (update status) |
| R-2.11.3 | Reject `~$` prefix, `_vti_`, `.lock` | implemented (update status — code already does this) |
| R-2.11.4 | Reject trailing dots, leading/trailing whitespace | implemented (update status — code already does this) |
| R-2.11.5 | Scanner-filtered files shall be recorded as actionable issues, not silently skipped | **new** |

### R-2 Sync — Case Collision (update R-2.12)

| ID | Requirement | Status |
|----|------------|--------|
| R-2.12.1 | Detect case collisions before upload, flag as actionable `case_collision` | planned (unchanged, but elevated priority due to silent data loss) |
| R-2.12.2 | Neither colliding file shall be uploaded until the collision is resolved | **new** |

### R-6.6 Observability (additions)

| ID | Requirement | Status |
|----|------------|--------|
| R-6.6.7 | Aggregate warnings: when >10 items share the same condition, log 1 WARN summary + individual DEBUG | **new** |
| R-6.6.8 | Individual retry attempts for transient errors shall be logged at DEBUG, not WARN | **new** |
| R-6.6.9 | Transient errors that resolve within retry budget shall not emit WARN-level logs | **new** |
| R-6.6.10 | Exhausted retries: single WARN with final error, attempt count, and next retry time | **new** |
| R-6.6.11 | Every failure shown to the user shall include plain-language reason AND concrete user action | **new** |
| R-6.6.11.1–11 | Per-error-type reason and action text (see Part 6 table) | **new** |

### R-6.7 Technical Requirements (updates)

| ID | Requirement | Status |
|----|------------|--------|
| R-6.7.14 | Parse HTTP 400 body for outage signatures, classify as service-scoped | planned (revise: add body parsing detail) |

### R-6.8 Network Resilience (additions)

| ID | Requirement | Status |
|----|------------|--------|
| R-6.8.4 | Treat 429 as account-scoped: share throttle gate across all drives per account | **new** |
| R-6.8.5 | Parse `RateLimit-Remaining` headers, proactively slow when < 20% remaining | **new** |
| R-6.8.6 | Honor `Retry-After` from both 429 and 503 responses | **new** |
| R-6.8.7 | Workers shall not block on retry backoff; actions returned to scheduler with `notBefore` timestamp | **new** |
| R-6.8.8 | Transport-level retry may block for short durations; executor-level retry shall not add blocking | **new** |

### Design Document Changes

| Document | Changes |
|----------|---------|
| `spec/design/retry.md` | Add scope-classified retry design (Design C). Add non-blocking worker requirement. Add scope clearance immediate re-queue. |
| `spec/design/sync-execution.md` | Update executor error classification (507 → actionable, os.ErrPermission → actionable). Add non-blocking withRetry design. Add local permission hierarchy. |
| `spec/design/sync-engine.md` | Add stale failure cleanup step (Design C). Add scope classifier component. Add aggregated logging. |
| `spec/design/sync-observation.md` | Add scanner-level issue recording for filtered files. |
| `spec/design/sync-store.md` | Add new issue types (`quota_exceeded`, `local_permission_denied`, `case_collision`, `disk_full`, `service_outage`, `rate_limited`). Add scope column to sync_failures schema. |
| `spec/design/graph-client.md` | Add shared per-account throttle gate. Add `RateLimit-*` header parsing. Add 503 Retry-After. |
| `spec/design/cli.md` | Update `issues` command for grouped display (R-2.3.7). Add per-error-type user action text. |
| `spec/reference/graph-api-quirks.md` | Add Microsoft throttling scope documentation (per-user, per-tenant, per-app-per-tenant limits with actual numbers). |
| `CLAUDE.md` | Add Failure Handling Principles to Eng Philosophy. |

### Code Changes

| Area | Files | Changes |
|------|-------|---------|
| **Error classification** | `executor.go` | Add `errClassActionable`. Detect `os.ErrPermission`. 507 → actionable. |
| **Transport retry** | `graph/errors.go` | 507 → not retryable at transport level. |
| **Shared throttle gate** | `graph/client.go` | Extract throttle state to `AccountThrottle` shared across clients. |
| **503 Retry-After** | `graph/client.go` | Parse `Retry-After` for 503 (not just 429). |
| **RateLimit headers** | `graph/client.go` | Parse `RateLimit-Remaining`, proactive slowdown. |
| **Non-blocking retry** | `executor.go`, `tracker.go` | Return actions to tracker with `NotBefore`. Tracker priority queue. |
| **Scope classifier** | `sync/scope.go` (new) | Event-driven scope classification. Probe scheduling. |
| **Scanner issue recording** | `scanner.go`, `engine.go` | Scanner reports skipped files. Engine batch-records as actionable issues. |
| **Stale failure cleanup** | `engine.go`, `baseline.go` | Engine clears stale upload failures after local observation. |
| **Local permission hierarchy** | `engine.go`, `permissions.go` | Detect `os.ErrPermission`, check directory accessibility, suppress subtree. |
| **Aggregated logging** | `engine.go`, `executor.go` | Batch warning aggregation with summary + DEBUG detail. |
| **Case collision detection** | `upload_validation.go` or `scanner.go` | Case-insensitive name map per directory. |
| **Issues display** | `issues.go` | Grouped output for >10 items. Per-error user action text. |
| **Download/delete failure cleanup** | `baseline.go` | Extend upload-success cleanup to cover download/delete success. |
| **New issue types** | `upload_validation.go`, `baseline.go` | `quota_exceeded`, `local_permission_denied`, `case_collision`, `disk_full`, `service_outage`. Update `isActionableIssue`. |

### Test Changes

Every requirement above needs corresponding tests. Key test areas:

| Test | Validates |
|------|-----------|
| 507 classified as actionable, not retried at transport level | R-2.10.1 |
| Upload success clears sync_failures (already exists), download/delete success also clears | Gap fix |
| Scanner-filtered files appear in sync_failures | R-2.11.5 |
| Stale failures auto-cleared when file disappears from scanner | R-2.10.2 |
| Scope escalation: 3 consecutive 507 → account block | R-2.10.3 |
| Scope clearance: probe succeeds → immediate re-queue | R-2.10.5 |
| Shared throttle gate across drives | R-6.8.4 |
| Non-blocking worker: action returned to tracker, worker picks up next | R-6.8.7 |
| Local permission denied → actionable, not retried | R-2.10.12 |
| Directory-level permission issue suppresses children | R-2.10.12 |
| Permission recheck clears resolved issues | R-2.10.13 |
| Case collision detected before upload | R-2.12.1 |
| Aggregated logging: >10 same-type warnings → 1 summary | R-6.6.7 |
| Individual retries at DEBUG, exhausted at WARN | R-6.6.8, R-6.6.10 |
| Issues display groups >10 items | R-2.3.7 |
| Per-error-type user action text present | R-6.6.11 |
| 503 Retry-After honored | R-6.8.6 |
| RateLimit-Remaining proactive slowdown | R-6.8.5 |
