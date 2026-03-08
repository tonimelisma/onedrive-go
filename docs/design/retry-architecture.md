# Retry & Error Persistence Architecture

## Overview

This document maps every retry and backoff mechanism in the codebase, every table where persistent errors are stored, and the CLI commands that surface them. It identifies architectural asymmetries and proposes a from-first-principles redesign.

For the delta token advancement bug and its interaction with failure tracking, see [failures.md](failures.md). For Graph API behavioral issues and their workarounds, see [ci_issues.md](ci_issues.md).

### Execution Model

The sync engine in watch mode is **event-driven, not cycle-based**. There are no discrete "sync cycles." Three independent subsystems run concurrently at their own pace (see [concurrent-execution.md](concurrent-execution.md)):

1. **Observers** — emit `ChangeEvent`s continuously (local: inotify/FSEvents; remote: delta polling)
2. **Buffer + Planner** — debounce events (2s quiet period), plan action batches, dispatch to tracker
3. **Worker pool** — drain actions from tracker, execute, report results

Multiple batches can be in-flight simultaneously. `processBatch()` returns immediately after dispatching actions — it does not wait for the previous batch to complete. New events are accepted continuously. The `cycleID` field on `TrackedAction` exists in the tracker infrastructure but is always passed as `""` in watch mode; `CycleDone()` and `CleanupCycle()` are never called from production watch-mode code.

`RunOnce` (one-shot mode) is the only place with a discrete observe→plan→execute→wait flow. The retry architecture described here applies to both modes, but the daemon (watch) mode is the primary concern.

---

## 1. The Retry Cascade

Six independent retry layers absorb failures at different scopes. Each layer has its own constants, backoff implementation, and retry eligibility rules. Each operates on individual items/actions — there is no batch-level or cycle-level retry.

### Layer 1: HTTP Transport (`internal/graph/client.go`)

Every Graph API call passes through `doRetry()` (authenticated) or `doPreAuthRetry()` (pre-authenticated URLs for downloads/uploads).

| Parameter | Value |
|-----------|-------|
| Max retries | 5 |
| Base backoff | 1s |
| Max backoff | 60s |
| Multiplier | 2x |
| Jitter | +/-25% |
| Retry-After | Honored verbatim on 429 |

**Retryable:** network errors, 408, 429, 500, 502, 503, 504, 509.

**Not retryable:** all other status codes (400, 401, 403, 404, 409, 410, 412, 423, 507). These are returned immediately to the caller.

**Implementation:** `retryBackoff()` (line ~388) reads `Retry-After` for 429; `calcBackoff()` (line ~404) computes exponential backoff with jitter for everything else.

### Layer 2: Drive Discovery (`internal/graph/drives.go`)

Domain-specific retry for transient 403 on `/me/drives` during Azure AD token propagation (see [ci_issues.md §13](ci_issues.md#13-transient-403-on-medrives-token-propagation-delay)).

| Parameter | Value |
|-----------|-------|
| Max retries | 3 |
| Backoff | Uses `calcBackoff()` from Layer 1 |

**Retryable:** 403 only. All other errors (including non-GraphError) fail immediately. Design decision DP-11 documents why this is domain-specific — other 403s (permission denied, retention policy) are permanent.

### Layer 3: Executor Action Retry (`internal/sync/executor.go`)

Each sync action (download, upload, delete, mkdir, move) gets up to 3 retries at the action level. This layer sits above the transport layer — each attempt re-enters Layer 1 for its HTTP calls.

| Parameter | Value |
|-----------|-------|
| Max retries | 3 |
| Base backoff | 1s |
| Multiplier | 2x |
| Jitter | +/-25% |

**Error classification** (`classifyError`, `classifyStatusCode`):

| Class | HTTP Codes | Behavior |
|-------|-----------|----------|
| `errClassRetryable` | 404, 408, 412, 429, 509, all >= 500 | Retry with backoff |
| `errClassSkip` | 423 (Locked), 403, 409, filesystem errors | No retry, record failure |
| `errClassFatal` | 401, 507, context canceled/deadline | Fail action immediately, report to result channel |

Note: `errClassFatal` does **not** abort the engine or stop other in-flight actions. In watch mode, it simply fails the individual action. Other actions and future batches continue unaffected. The "fatal" label refers to the action scope, not the system scope.

**Key override:** Layer 1 treats 404 as non-retryable, but Layer 3 classifies it as retryable. This handles Graph API transient 404s on valid resources (see [ci_issues.md §1](ci_issues.md#1-transient-404-on-valid-resources)).

**Implementation:** `withRetry()` (line ~452) with `calcExecBackoff()` (line ~484).

### Layer 4: Download Hash Retry (`internal/driveops/transfer_manager.go`)

Orthogonal to Layers 1-3. After a successful download, if the file's hash doesn't match the expected hash, the entire download is discarded and retried.

| Parameter | Value |
|-----------|-------|
| Max retries | 2 |
| Backoff | None (immediate re-download) |

**Cause:** server-side replication lag where content hasn't converged across replicas.

**Implementation:** `downloadWithHashRetry()` (line ~189).

### Layer 5: Observer Watch Loops

Both local and remote observers run infinite loops with backoff on errors. These are independent of action execution — they operate on the observation side of the pipeline, not the execution side.

**Local observer** (`internal/sync/observer_local.go`, `observer_local_handlers.go`):

| Parameter | Value |
|-----------|-------|
| Initial backoff | 1s |
| Max backoff | 30s |
| Multiplier | 2x |
| Reset | On successful event or safety scan |
| Coalesce retry | 3 attempts on `errFileChangedDuringHash` |

**Remote observer** (`internal/sync/observer_remote.go`):

| Parameter | Value |
|-----------|-------|
| Initial backoff | 5s |
| Max backoff | poll_interval |
| Multiplier | 2x |
| Max consecutive | 10 (overflow guard) |
| Reset | On successful poll |

**Special:** ErrGone (HTTP 410 — delta token expired) resets the delta token and triggers a full re-enumeration. See [ci_issues.md §7](ci_issues.md#7-http-410-gone--delta-token-expired).

### Layer 6: Reconciler / Failure Retrier (`internal/sync/reconciler.go`)

Periodic sweep that re-injects persistently failed items into the event pipeline. Runs on three triggers: explicit kick signals, a 2-minute safety ticker, and a timer armed for the earliest `next_retry_at`. The reconciler is an independent subsystem — it doesn't know about batches or observers. It reads the database and emits events.

| Parameter | Value |
|-----------|-------|
| Escalation threshold | 10 consecutive failures |
| Safety interval | 2 minutes |
| Backoff (per-item) | 30s * 2^failure_count, capped at 1 hour, +/-25% jitter |

**Implementation:** `computeNextRetry()` in `baseline.go` (line ~1145). Used by both `remote_state` and `local_issues` tables.

**Two sweep paths:**
1. `reconcile()` — queries `remote_state` for rows with `next_retry_at <= now`, re-injects into sync pipeline. Escalates to `sync_failure` conflict after 10 failures.
2. `reconcileLocalIssues()` — queries `local_issues` for rows with expired backoff, re-injects or marks `permanently_failed`.

---

## 2. Combined Retry Budget

A single item's failure can trigger retries across multiple layers. Since the system is event-driven, these retries happen per-action, not per-cycle:

| Status Code | Layer 1 (Transport) | Layer 3 (Executor) | Layer 6 (Reconciler) | Total HTTP Requests (Worst Case) |
|-------------|--------------------|--------------------|---------------------|----------------------------------|
| Network error | 5 retries | Skip (not HTTP) | N/A | 6 |
| 401 | No retry | Fatal (action only) | N/A | 1 |
| 404 | No retry | 3 retries | 10 re-injections | 4 per attempt, 40 total |
| 429 | 5 retries | 3 retries | 10 re-injections | 24 per attempt, 240 total |
| 500-504, 509 | 5 retries | 3 retries | 10 re-injections | 24 per attempt, 240 total |
| 507 | No retry | Fatal (action only) | N/A | 1 |

**Full cascade for a persistent 503:**
```
Layer 1: 6 HTTP attempts (1 + 5 retries, ~2 minutes with backoff)
  × Layer 3: 4 action attempts (1 + 3 retries, ~7 seconds between)
    = 24 HTTP requests per action execution
  × Layer 6: 10 reconciler re-injections (30s, 1m, 2m, 4m, 8m, 16m, 32m, 1h, 1h, 1h)
    = 240 HTTP requests over ~4.5 hours
  → EscalateToConflict() — human resolves via CLI
```

Note: each reconciler re-injection is independent. Item D can be retrying its 7th attempt while item E is on its 2nd. The reconciler doesn't batch items — it re-injects each one individually as its `next_retry_at` expires.

---

## 3. Persistent Error Storage

When the executor exhausts its 3 retries for an action, the failure becomes durable state in SQLite. Three tables store different categories of persistent errors.

### 3.1 `remote_state` — Download/Delete Failure Tracking

**Schema** (from `migrations/00001_consolidated_schema.sql`):
```sql
CREATE TABLE remote_state (
    drive_id      TEXT    NOT NULL,
    item_id       TEXT    NOT NULL,
    path          TEXT    NOT NULL,
    sync_status   TEXT    NOT NULL DEFAULT 'pending_download'
                  CHECK(sync_status IN (
                      'pending_download', 'downloading', 'download_failed',
                      'synced',
                      'pending_delete', 'deleting', 'delete_failed', 'deleted',
                      'filtered')),
    failure_count INTEGER NOT NULL DEFAULT 0,
    next_retry_at INTEGER,
    last_error    TEXT,
    http_status   INTEGER,
    -- ... other columns omitted
    PRIMARY KEY (drive_id, item_id)
);
```

**Purpose:** Full mirror of every item the delta API has reported. Failure columns track download and delete failures.

**Populated by:** `RecordFailure(ctx, path, errMsg, httpStatus)` in `baseline.go` (line ~1081). Called from `drainWorkerResults()` in `engine.go` after the executor exhausts its 3 retries. Transitions: `downloading → download_failed`, `deleting → delete_failed`.

**Retry scheduling:** `computeNextRetry()` computes the next attempt time. The reconciler queries `ListFailedForRetry(now)` and re-injects eligible items.

**Escalation:** After `failure_count >= 10`, `EscalateToConflict()` creates a `sync_failure` conflict and NULLs `next_retry_at` (stops retry scheduling). The row remains in `*_failed` status.

**CLI visibility:** Aggregate count via `status` command ("Pending: N items"). Individual failures visible in sync command output during runs ("Failed: N"). **No dedicated CLI command to inspect individual remote_state failures.** Users cannot query which specific files are failing, what errors they're hitting, or how many retries remain — until escalation to conflicts after 10 failures.

### 3.2 `local_issues` — Upload-Side Failure Tracking

**Schema:**
```sql
CREATE TABLE local_issues (
    path          TEXT    PRIMARY KEY,
    issue_type    TEXT    NOT NULL
                  CHECK(issue_type IN (
                      'invalid_filename', 'path_too_long', 'file_too_large',
                      'permission_denied', 'upload_failed', 'quota_exceeded',
                      'locked', 'sharepoint_restriction')),
    sync_status   TEXT    NOT NULL DEFAULT 'pending_upload'
                  CHECK(sync_status IN (
                      'pending_upload', 'uploading', 'upload_failed',
                      'permanently_failed', 'resolved')),
    failure_count INTEGER NOT NULL DEFAULT 0,
    next_retry_at INTEGER,
    last_error    TEXT,
    http_status   INTEGER,
    first_seen_at INTEGER NOT NULL,
    last_seen_at  INTEGER NOT NULL,
    file_size     INTEGER,
    local_hash    TEXT
);
```

**Purpose:** Track files that cannot be uploaded, with classification by issue type.

**Two categories:**

| Category | Issue Types | Behavior |
|----------|------------|----------|
| **Permanent** | `invalid_filename`, `path_too_long`, `file_too_large` | Immediately `permanently_failed`, `next_retry_at = NULL`. Never retried — these are validation failures that won't change without user action. |
| **Transient** | `upload_failed`, `permission_denied`, `quota_exceeded`, `locked`, `sharepoint_restriction` | Same 30s * 2^n backoff as remote_state. Retried by `reconcileLocalIssues()`. |

**Populated by:** `RecordLocalIssue()` in `baseline.go` (line ~1630). Two call sites:
1. **Pre-upload validation** (`upload_validation.go`): checks filename validity, path length, file size before any HTTP call.
2. **Post-execution failure** (`drainWorkerResults()` in `engine.go`): records upload execution failures.

**CLI visibility:** `issues list` shows every row with full detail (path, issue type, sync status, failure count, last error, HTTP status, file size, first/last seen). Supports `--json` output. `issues clear [path]` removes specific issues. `issues clear --all` removes all resolved issues.

### 3.3 `conflicts` — Content Conflicts and Escalated Failures

**Schema:**
```sql
CREATE TABLE conflicts (
    id              TEXT    PRIMARY KEY,
    drive_id        TEXT    NOT NULL,
    item_id         TEXT,
    path            TEXT    NOT NULL,
    conflict_type   TEXT    NOT NULL CHECK(conflict_type IN (
                                'edit_edit', 'edit_delete', 'create_create', 'sync_failure'
                            )),
    resolution      TEXT    NOT NULL DEFAULT 'unresolved'
                            CHECK(resolution IN (
                                'unresolved', 'keep_both', 'keep_local',
                                'keep_remote', 'manual'
                            )),
    detected_at     INTEGER NOT NULL,
    local_hash      TEXT,
    remote_hash     TEXT,
    resolved_at     INTEGER,
    resolved_by     TEXT    CHECK(resolved_by IN ('user', 'auto') OR resolved_by IS NULL)
);
```

**Purpose:** Record situations requiring human judgment.

**Two distinct sources:**

| Type | Source | Meaning |
|------|--------|---------|
| `edit_edit` | Planner | Both local and remote modified the same file since baseline |
| `edit_delete` | Planner | One side edited, the other deleted |
| `create_create` | Planner | Both sides created a file at the same path |
| `sync_failure` | Reconciler escalation | A `remote_state` failure exceeded the 10-attempt threshold |

**Never auto-retried.** All conflicts require human resolution via the `resolve` command (`keep_local`, `keep_remote`, `keep_both`, `manual`).

**CLI visibility:** `conflicts list` shows unresolved conflicts (default). `conflicts --history` shows all including resolved. `resolve` command applies resolution.

---

## 4. The Asymmetry Problem

### 4.1 Upload vs Download Visibility

Error persistence is asymmetric by direction:

```
Upload failure path:
  executor fails
    → RecordLocalIssue() → local_issues table
    → issues CLI (immediately visible, queryable, clearable)

Download/delete failure path:
  executor fails
    → RecordFailure() → remote_state table
    → ??? (no dedicated CLI)
    → ... 10 failures over ~4.5 hours ...
    → EscalateToConflict() → conflicts table
    → conflicts CLI (visible only after escalation)
```

Users can immediately see which uploads are failing and why (`issues list`). They cannot see which downloads are failing until the system gives up after 10 attempts and escalates to a conflict.

### 4.2 Dual Writes for Uploads

Upload failures trigger writes to both tables. In `drainWorkerResults()` (`engine.go`, line ~884):

1. `RecordFailure()` → writes to `remote_state` (failure tracking)
2. `RecordLocalIssue("upload_failed")` → writes to `local_issues` (user visibility)

The `remote_state` write drives retry scheduling. The `local_issues` write provides CLI visibility. The same failure exists in two tables simultaneously with independently tracked failure counts.

### 4.3 `sync_failure` Conflated with True Conflicts

The `conflicts` table mixes two fundamentally different problems:

| Type | User Action | What Happened |
|------|-------------|---------------|
| `edit_edit`, `edit_delete`, `create_create` | Choose which version to keep | Genuine content conflict — both sides changed |
| `sync_failure` | Debug infrastructure issue | Download/delete failed 10 times — network, permissions, server error |

The `conflicts` CLI shows both, but they require different responses. A content conflict needs "keep local or remote?" A sync failure needs "what's the error? Is the server back up? Do I have permission?"

---

## 5. CLI Surface Area

### `status` Command

Shows aggregate counts per drive:
- **Pending:** count of `remote_state` rows with `sync_status NOT IN ('synced', 'deleted', 'filtered')` — includes both pending and failed items, without distinguishing them.
- **Conflicts:** count of unresolved conflicts.
- **Upload Issues:** count of `local_issues` rows with `sync_status != 'resolved'`.

Does not show individual items, error messages, retry state, or failure counts.

### `issues` Command

Subcommands:
- `issues list` — all `local_issues` rows with full detail. Supports `--json`.
- `issues clear [path]` — delete specific issue.
- `issues clear --all` — delete all resolved issues.

Shows: path, issue type, sync status, failure count, last error, HTTP status, file size, first/last seen.

### `conflicts` Command

Subcommands:
- `conflicts list` — unresolved conflicts. Supports `--json`.
- `conflicts --history` — all conflicts including resolved.

Shows: ID, path, conflict type, detected at, local/remote hashes, resolution, resolved at/by.

### `resolve` Command

Applies resolution to a conflict: `resolve <id> keep_local|keep_remote|keep_both|manual`.

### Missing: `failures` Command

There is no CLI command to inspect `remote_state` failures. The gap covers:
- Which files are failing to download/delete
- What error each file is hitting
- How many retries have been consumed
- When the next retry is scheduled
- Whether the item is approaching the escalation threshold

---

## 6. Backoff Implementations

Six independent implementations of the same formula (`base * multiplier^attempt + jitter`):

| Location | Constants | Shared? |
|----------|-----------|---------|
| `graph/client.go` `calcBackoff()` | 1s base, 60s cap, 2x, 25% jitter | Used by Layer 1 and Layer 2 |
| `sync/executor.go` `calcExecBackoff()` | 1s base, 2x, 25% jitter | Layer 3 only |
| `sync/baseline.go` `computeNextRetry()` | 30s base, 3600s cap, 2x, 25% jitter | Layer 6 only |
| `sync/observer_local.go` inline | 1s base, 30s cap, 2x, no jitter | Layer 5 local only |
| `sync/observer_remote.go` `advanceBackoff()` | 5s base, poll_interval cap, 2x, no jitter | Layer 5 remote only |
| `e2e/e2e_test.go` `pollBackoff()` | 500ms base, 4s cap, 2x (bit shift), no jitter | Test only |

All are variations of exponential backoff. None share a common type or implementation.

---

## 7. From First Principles: Proposed Redesign

### 7.1 Unified Retry Policy Type

Replace six independent implementations with a single type:

```go
type RetryPolicy struct {
    MaxAttempts int
    Base        time.Duration
    Max         time.Duration
    Multiplier  float64
    Jitter      float64 // 0.0-1.0
}

func (p RetryPolicy) Delay(attempt int) time.Duration { ... }
func (p RetryPolicy) Do(ctx context.Context, fn func() error) error { ... }
```

Named instances:
```go
var (
    TransportRetry   = RetryPolicy{MaxAttempts: 5, Base: 1*time.Second, Max: 60*time.Second, Multiplier: 2, Jitter: 0.25}
    ActionRetry      = RetryPolicy{MaxAttempts: 3, Base: 1*time.Second, Max: 30*time.Second, Multiplier: 2, Jitter: 0.25}
    ReconcileRetry   = RetryPolicy{MaxAttempts: 10, Base: 30*time.Second, Max: 1*time.Hour, Multiplier: 2, Jitter: 0.25}
    WatchLocalRetry  = RetryPolicy{MaxAttempts: 0, Base: 1*time.Second, Max: 30*time.Second, Multiplier: 2, Jitter: 0}
    WatchRemoteRetry = RetryPolicy{MaxAttempts: 0, Base: 5*time.Second, Max: 5*time.Minute, Multiplier: 2, Jitter: 0}
)
```

One implementation. One test. Configurable per-use-case. `MaxAttempts: 0` means infinite (for watch loops).

### 7.2 Three Clean Tiers

The current six layers have ambiguous boundaries. Layer 1 and Layer 3 disagree on 404 retryability. Layer 4 is orthogonal to Layer 3 but embedded in the transfer manager.

Proposed three-tier model:

| Tier | Scope | Responsibility | Error Contract |
|------|-------|---------------|----------------|
| **Transport** | Single HTTP round-trip | Network, throttling, server errors | Returns typed errors: `*ThrottleError`, `*ServerError`, `*ClientError`. Never surfaces a retryable error. |
| **Action** | Single sync action | Semantic retries: hash mismatch, transient 404, lock wait | Returns `ActionResult{Success bool, Retryable bool, Err error}`. No HTTP awareness. |
| **Scheduler** | Per-item, across time | Persistent retry with backoff, escalation | Reads/writes DB. Owns the retry budget and escalation threshold. |

Key principle: **each tier fully resolves its failure class.** The transport never leaks a retryable error to the action tier. The action tier never leaks a retryable error to the scheduler. Each tier's error contract with the next tier is clean and typed.

The scheduler tier is not "cross-cycle" — there are no cycles. It operates on individual items with their own independent retry timers. Each item has its own `failure_count` and `next_retry_at`. The reconciler wakes when any item's timer expires and re-injects that single item.

The current override (Layer 3 re-classifying 404 as retryable despite Layer 1 treating it as non-retryable) would be eliminated: the transport tier would return a typed `*ClientError{Code: 404}`, and the action tier would decide whether to retry based on the operation context (sync actions retry 404; CLI file operations don't).

### 7.3 Unified Failures Table

Merge `remote_state` failure columns and `local_issues` into a single table:

```sql
CREATE TABLE sync_failures (
    path           TEXT    PRIMARY KEY,
    direction      TEXT    NOT NULL CHECK(direction IN ('download', 'upload', 'delete')),
    category       TEXT    NOT NULL CHECK(category IN ('transient', 'permanent')),
    issue_type     TEXT,
    failure_count  INTEGER NOT NULL DEFAULT 0,
    next_retry_at  INTEGER,
    last_error     TEXT,
    http_status    INTEGER,
    first_seen_at  INTEGER NOT NULL,
    last_seen_at   INTEGER NOT NULL,
    file_size      INTEGER,
    item_id        TEXT,
    drive_id       TEXT
);
```

Benefits:
- **One CLI command** (`failures list`) covers all directions. Filterable by `--direction`, `--category`.
- **One reconciler sweep** instead of two separate paths.
- **No dual writes** — upload failures write to one place.
- **Symmetric visibility** — download failures are just as inspectable as upload failures.
- The `remote_state` table retains its sync_status state machine but delegates failure tracking to the failures table via a foreign key or path lookup.

### 7.4 Separate Conflicts from Exhausted Retries

Remove `sync_failure` from the `conflicts` table. Exhausted retries become `category = 'permanent'` in the unified failures table.

The `conflicts` table is reserved for genuine content conflicts requiring human judgment about which version to keep. The `failures` CLI shows permanent failures with actionable context:

```
$ onedrive-go failures list
PATH                    DIR       ATTEMPTS  NEXT RETRY  ERROR
Documents/report.xlsx   download  7/10      in 8m 12s   503 Service Unavailable
Photos/large.raw        download  3/10      in 1m 04s   hash mismatch
backup/CON.txt          upload    permanent             invalid filename

$ onedrive-go conflicts list
PATH                    TYPE        DETECTED
notes.md                edit_edit   2024-03-07 14:23
old-report.xlsx         edit_delete 2024-03-07 14:25
```

Each CLI command shows one thing. Users know what action to take: `failures` need debugging or waiting; `conflicts` need version selection.

### 7.5 Circuit Breaker

The current system retries each item independently. If OneDrive returns 503 for every request, each of 100 pending items independently burns through 24 HTTP requests (5 transport retries × 4 executor attempts) before recording a failure. That's 2,400 HTTP requests against a service that is clearly down.

A circuit breaker in the Graph client:

```go
type CircuitBreaker struct {
    FailureThreshold int           // e.g., 5 failures
    Window           time.Duration // e.g., 30 seconds
    CooldownPeriod   time.Duration // e.g., 60 seconds
}
```

**States:**
- **Closed** (normal): requests flow through. Track failure rate.
- **Open** (tripped after N failures in M seconds): all requests fail-fast with `ErrCircuitOpen`. The sync engine pauses with a clear status message.
- **Half-open** (after cooldown): let one probe request through. Success → closed. Failure → open with reset cooldown.

Benefits:
- Prevents thundering herd of retries during outages.
- Preserves retry budgets for when the API recovers.
- Better UX: "OneDrive API unavailable, pausing sync" vs "237 items failed".
- Aligns with the Graph API 400 "ObjectHandle is Invalid" outage pattern ([ci_issues.md §15](ci_issues.md#15-http-400-objecthandle-is-invalid-microsoft-outage-pattern)) where retrying was pointless for 3.5 hours.

### 7.6 Retry Budget Visibility

The `status` command should show retry state:

```
Sync: active (last completed 30s ago)
  Synced:      1,234 files
  Pending:        12 items
  Failing:         3 items (2 retrying, 1 permanent)
    - Documents/report.xlsx: download failed 7/10 (503, next retry: 8m)
    - Photos/large.raw: download failed 3/10 (hash mismatch, next retry: 1m)
    - backup/CON.txt: upload permanent (invalid filename)
  Conflicts:       1 (edit_edit)
  API health:  healthy (last 100 requests: 98 OK, 2 retried)
```

The circuit breaker state, per-item retry progress, and aggregate API health belong in the status view. Currently, the only way to see individual failure state is to query the SQLite database directly.

### 7.7 Remove Dead Cycle Infrastructure

The tracker's `cycleID` field, `CycleDone()`, `CleanupCycle()`, `registerCycleLocked()`, and the `cycles`/`cycleLookup` maps are unused in watch mode — every call passes `""`. The `cycleTracker` type and its tests exist but serve no production purpose.

Options:
1. **Remove entirely** if one-shot mode doesn't need it (currently also passes `""`).
2. **Gate behind a build tag** if future use is planned.
3. **Leave as-is** — it's inert code with test coverage. Low risk, but misleading.

The misleading naming is the real cost. Anyone reading the tracker code assumes cycles are a fundamental concept. They aren't.

---

## 8. Phase 8.0 Impact: WebSocket Remote Observer

Phase 8.0 (roadmap) replaces the 5-minute polling timer with a WebSocket-triggered delta call, keeping polling as a fallback. This section analyzes how that refactor interacts with every aspect of the retry architecture.

### 8.1 What WebSocket Actually Does

The Graph API WebSocket endpoint (`/v1.0/drives/{driveId}/root/subscriptions/socketIo`) is a **notification signal only** — it tells the client "something changed on this drive" but not what changed. The sync engine must still call the delta API to get actual changes. From [api-analysis.md §6.18](../tier1-research/api-analysis.md):

> The notification only signals that something changed; the sync engine must still call the delta API to get the actual changes. [...] Not available as a standalone change detection mechanism — always use with delta polling as a fallback.

The data flow is unchanged:

```
Current:   timer tick ──────────────────→ FullDelta() → events → buffer → planner → workers
Phase 8.0: websocket nudge OR timer tick → FullDelta() → events → buffer → planner → workers
```

The trigger changes; everything downstream is identical. The system remains event-driven — WebSocket nudges are just another event source feeding the same pipeline.

### 8.2 New Failure Domains

WebSocket introduces three failure domains that don't exist today:

| Component | Failure Modes | Retry Needed |
|-----------|--------------|--------------|
| **WebSocket connection** | Network drop, server close, firewall block, TLS error | Reconnection with backoff |
| **Subscription lifecycle** | Expiration (3-day default), 308 redirect bug ([tier1 §7](../tier1-research/issues-graph-api-bugs.md#7-308-permanent-redirect-without-location-header-webhook-subscription)), creation failure | Renewal timer + delete-and-recreate fallback |
| **Nudge delivery** | Missed nudges, duplicate nudges, stale nudges | Polling fallback (existing) |

The tier1 research documents a known Graph API bug: webhook subscription renewal returns HTTP 308 without a `Location` header on Personal accounts. The abraunegg/onedrive workaround is to delete and recreate the subscription on any renewal failure. This needs its own retry policy.

### 8.3 Impact on Each Retry Layer

**Layer 1 (HTTP Transport):** Unchanged. Delta calls still go through `doRetry()`. WebSocket is a separate connection, not an HTTP request/response.

**Layer 2 (Drive Discovery):** Unchanged.

**Layer 3 (Executor):** Unchanged. The executor doesn't know or care what triggered the delta call.

**Layer 4 (Hash Retry):** Unchanged.

**Layer 5 (Observer Watch Loops):** Major changes. The remote observer currently has one backoff path (delta poll error → exponential backoff). With WebSocket, it needs three:

```
RemoteObserver.Watch() with WebSocket:
  ├── Delta poll backoff (existing): 5s → poll_interval, 2x
  ├── WebSocket connection backoff (new): reconnect with backoff on drop
  └── Subscription renewal backoff (new): retry/recreate on expiration or 308
```

The observer must run WebSocket and polling concurrently — WebSocket for low-latency notification, polling as a safety net for missed nudges. When WebSocket is healthy, polling interval can be extended (e.g., 30 minutes instead of 5). When WebSocket drops, polling interval tightens back to the configured default.

**Layer 6 (Reconciler):** Unchanged. Persistent failures are scheduled by `next_retry_at` regardless of the trigger mechanism.

### 8.4 Circuit Breaker Becomes Required

With polling, the system makes one delta call per 5-minute interval — a slow burn during outages. With WebSocket, every remote change triggers an immediate delta call. If the API is returning errors:

```
Without circuit breaker:
  websocket nudge → delta call → 503 → Layer 1 retries 5× → fails
  websocket nudge → delta call → 503 → Layer 1 retries 5× → fails
  websocket nudge → delta call → 503 → ...
  (tight loop: potentially dozens of nudges per minute)

With circuit breaker:
  websocket nudge → delta call → 503 → Layer 1 retries 5× → fails
  websocket nudge → delta call → 503 → circuit opens
  websocket nudge → ErrCircuitOpen → skip, log "API unavailable"
  ... cooldown ...
  websocket nudge → half-open → probe → 503 → circuit stays open
  ... cooldown ...
  websocket nudge → half-open → probe → 200 → circuit closes
```

Without a circuit breaker, WebSocket nudges during an outage (like the 3.5-hour ObjectHandle incident, [ci_issues.md §15](ci_issues.md#15-http-400-objecthandle-is-invalid-microsoft-outage-pattern)) would generate thousands of failed delta calls instead of the current ~42 (one per 5-minute poll). The circuit breaker transforms this from O(nudges) to O(probes).

### 8.5 Delta Token Bug Fires Faster

The delta token advancement bug ([failures.md](failures.md)) has the timeline: an action for item D fails → a subsequent batch containing different items succeeds → the in-memory delta token (which has already advanced past D) gets committed to the database. Currently the gap between batches is bounded by the poll interval (~5 minutes). With WebSocket nudges, a burst of changes can produce multiple batches in seconds:

```
Current (polling):
  T+0:00  Batch A: item D fails (423 locked) → token T' NOT committed
  T+5:00  Batch B: items F, G succeed → token T'' committed past D
  Gap: 5 minutes

Phase 8.0 (WebSocket):
  T+0:00  Batch A: item D fails (423 locked) → token T' NOT committed
  T+0:03  WebSocket nudge → Batch B: items F, G succeed → token T'' committed
  Gap: 3 seconds
```

**The delta token bug must be fixed before Phase 8.0.** The fix (tracking per-batch tokens rather than a global in-memory token) is independent of WebSocket but becomes urgent when the batch frequency increases by two orders of magnitude.

### 8.6 Zero-Event Token Guard Interaction

The zero-event guard ([ci_issues.md §20](ci_issues.md#20-delta-token-advancement-on-zero-event-responses)) prevents token advancement when delta returns 0 events. With WebSocket:

- **Spurious nudges** (WebSocket fires but delta returns nothing): more frequent. The guard handles this correctly — 0 events → don't advance token → replay is O(1).
- **Rapid successive nudges**: WebSocket may fire multiple times for a batch of changes. Each nudge triggers a delta call. The first call returns the events; subsequent calls return 0 events until the next change. The guard prevents unnecessary token advancement on these trailing calls.

The guard's assumption ("replaying an empty delta window costs O(1)") holds. No changes needed.

### 8.7 New Retry Policy Instances

With the proposed unified `RetryPolicy` type, Phase 8.0 adds:

```go
var (
    // ... existing policies ...
    WebSocketConnRetry  = RetryPolicy{MaxAttempts: 0, Base: 1*time.Second, Max: 5*time.Minute, Multiplier: 2, Jitter: 0.25}
    SubscriptionRenew   = RetryPolicy{MaxAttempts: 3, Base: 5*time.Second, Max: 30*time.Second, Multiplier: 2, Jitter: 0.25}
)
```

Without the unified type, these would be two more independent backoff implementations (bringing the total to 8).

### 8.8 Graceful Degradation Model

WebSocket is an optimization, not a requirement. The remote observer must handle:

| WebSocket State | Behavior |
|----------------|----------|
| **Connected** | Delta on nudge. Poll interval extended to 30 minutes (safety net). |
| **Reconnecting** | Poll interval tightened to configured default (5 minutes). Reconnection backoff running. |
| **Unavailable** (firewall, unsupported) | Poll-only mode. `websocket = false` or auto-detected. No retry overhead. |
| **Subscription expired** | Delete and recreate. If recreation fails 3×, fall back to poll-only until next renewal attempt. |

The `status` command should surface WebSocket health:

```
Sync: active (WebSocket: connected, last nudge: 12s ago)
```

or:

```
Sync: active (WebSocket: reconnecting, fallback polling every 5m)
```

### 8.9 Summary: Prerequisites for Phase 8.0

Before implementing WebSocket, three items from the retry architecture should be addressed:

1. **Fix the delta token advancement bug** ([failures.md](failures.md)). WebSocket increases batch frequency by 100×, shrinking the bug's window from minutes to seconds.
2. **Implement a circuit breaker** (§7.5). Without it, WebSocket nudges during outages hammer the API at nudge rate instead of poll rate.
3. **Implement the unified `RetryPolicy` type** (§7.1). WebSocket adds 2 more backoff contexts. Without unification, that's 8 independent implementations.

---

## 9. Summary

### 9.1 Current State

The retry architecture is **correct and robust** — failures are durable, retried with increasing backoff, and eventually escalated to human attention. The system handles the full spectrum of Graph API failure modes documented in [ci_issues.md](ci_issues.md).

The system is event-driven with per-item retry tracking. Each item has independent `failure_count` and `next_retry_at` state. The reconciler re-injects items individually as their timers expire. There are no discrete sync cycles in watch mode — batches flow continuously through the pipeline.

### 9.2 Accidental Complexity

1. **Six independent backoff implementations** with duplicated formulas and subtly different parameter names.
2. **Asymmetric failure visibility** — uploads get the `issues` CLI immediately; downloads get nothing for up to 10 re-injections, then appear as "conflicts."
3. **`sync_failure` conflated with true conflicts** — infrastructure failures and content conflicts share a table and CLI, but require fundamentally different user responses.
4. **Dual writes for upload failures** — same failure recorded in both `remote_state` and `local_issues`.
5. **No global failure awareness** — each item retries independently with no circuit breaker for service-wide outages.
6. **Opaque retry state** — users cannot see which items are failing, why, or when the next retry is scheduled.
7. **Dead cycle infrastructure** — `cycleID`, `CycleDone()`, `CleanupCycle()` in the tracker are unused in watch mode, creating a misleading impression that cycles are a core concept.

### 9.3 Relationship to Other Design Docs

- [failures.md](failures.md) — Delta token advancement bug and its interaction with the failure tracker. Focuses on the specific mechanism by which download failures can cause permanent item loss in watch mode.
- [ci_issues.md](ci_issues.md) — Graph API behavioral issues that drive the retry design choices (transient 404, 403 propagation delay, ObjectHandle outages, delta lag, etc.).
- [remote-state-separation.md](remote-state-separation.md) — The `remote_state` table design, sync_status state machine, and 30-cell decision matrix for status transitions.
- [concurrent-execution.md](concurrent-execution.md) — Three-subsystem execution architecture: observers, transfer queue, baseline commits. Confirms the event-driven model with no synchronization barriers between batches.
- [observability.md](observability.md) — Daemon metrics and status design.
- [event-driven-rationale.md](event-driven-rationale.md) — Architectural decision record for the event-driven model. One-shot mode is "collect all events, then process as a batch" — the degenerate case of the continuous pipeline.
